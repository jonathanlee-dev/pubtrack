package pubmed

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const fetchBatchSize = 100

type Article struct {
	PMID             string   `json:"pmid"`
	Title            string   `json:"title"`
	Journal          string   `json:"journal"`
	Published        string   `json:"published,omitempty"`
	EntrezDate       string   `json:"entrez_date,omitempty"`
	DOI              string   `json:"doi,omitempty"`
	Authors          []string `json:"authors,omitempty"`
	Abstract         string   `json:"abstract,omitempty"`
	PublicationTypes []string `json:"publication_types,omitempty"`
}

func (a Article) URL() string { return "https://pubmed.ncbi.nlm.nih.gov/" + a.PMID + "/" }

func (a Article) IsErratum() bool {
	for _, t := range a.PublicationTypes {
		if t == "Published Erratum" {
			return true
		}
	}
	return false
}

// Fetch returns full records for pmids, in batches. A PMID that PubMed
// doesn't return is simply absent; callers decide what that means.
func (c *Client) Fetch(ctx context.Context, pmids []string) ([]Article, error) {
	var out []Article
	for start := 0; start < len(pmids); start += fetchBatchSize {
		batch := pmids[start:min(start+fetchBatchSize, len(pmids))]
		body, err := c.get(ctx, "efetch.fcgi", url.Values{
			"db":      {"pubmed"},
			"retmode": {"xml"},
			"id":      {strings.Join(batch, ",")},
		})
		if err != nil {
			return out, err
		}
		arts, err := ParseArticles(body)
		if err != nil {
			return out, err
		}
		out = append(out, arts...)
	}
	return out, nil
}

// ParseArticles parses efetch XML (retmode=xml). efetch has no JSON output;
// esummary does, but has no abstracts, so one XML call replaces two.
func ParseArticles(body []byte) ([]Article, error) {
	var set struct {
		XMLName  xml.Name
		Articles []xmlArticle `xml:"PubmedArticle"`
	}
	if err := xml.Unmarshal(body, &set); err != nil {
		return nil, fmt.Errorf("efetch: parsing XML: %w", err)
	}
	if set.XMLName.Local != "PubmedArticleSet" {
		return nil, fmt.Errorf("efetch: unexpected response <%s>: %s", set.XMLName.Local, snippet(body))
	}
	out := make([]Article, 0, len(set.Articles))
	for _, x := range set.Articles {
		out = append(out, x.toArticle())
	}
	return out, nil
}

// Paths are anchored at the article so that the reference list, which has
// its own ArticleIdList and DOIs for every cited paper, never matches.
type xmlArticle struct {
	PMID         string            `xml:"MedlineCitation>PMID"`
	Title        innerText         `xml:"MedlineCitation>Article>ArticleTitle"`
	Journal      string            `xml:"MedlineCitation>Article>Journal>Title"`
	JournalISO   string            `xml:"MedlineCitation>Article>Journal>ISOAbbreviation"`
	PubDate      xmlDate           `xml:"MedlineCitation>Article>Journal>JournalIssue>PubDate"`
	ArticleDates []xmlDate         `xml:"MedlineCitation>Article>ArticleDate"`
	Abstract     []xmlAbstractText `xml:"MedlineCitation>Article>Abstract>AbstractText"`
	Authors      []xmlAuthor       `xml:"MedlineCitation>Article>AuthorList>Author"`
	ELocations   []xmlELocation    `xml:"MedlineCitation>Article>ELocationID"`
	PubTypes     []string          `xml:"MedlineCitation>Article>PublicationTypeList>PublicationType"`
	ArticleIDs   []xmlArticleID    `xml:"PubmedData>ArticleIdList>ArticleId"`
	History      []xmlHistoryDate  `xml:"PubmedData>History>PubMedPubDate"`
}

type xmlDate struct {
	Year        string `xml:"Year"`
	Month       string `xml:"Month"`
	Day         string `xml:"Day"`
	MedlineDate string `xml:"MedlineDate"`
}

type xmlHistoryDate struct {
	Status string `xml:"PubStatus,attr"`
	Year   string `xml:"Year"`
	Month  string `xml:"Month"`
	Day    string `xml:"Day"`
}

type xmlAuthor struct {
	LastName       string `xml:"LastName"`
	Initials       string `xml:"Initials"`
	CollectiveName string `xml:"CollectiveName"`
}

type xmlELocation struct {
	Type  string `xml:"EIdType,attr"`
	Value string `xml:",chardata"`
}

type xmlArticleID struct {
	Type  string `xml:"IdType,attr"`
	Value string `xml:",chardata"`
}

// innerText collects all text inside an element, including text inside
// inline markup. Titles and abstracts contain <i>, <sup>, and <b>, which a
// plain chardata field would silently drop.
type innerText string

func (t *innerText) UnmarshalXML(d *xml.Decoder, _ xml.StartElement) error {
	s, err := collectText(d)
	*t = innerText(s)
	return err
}

type xmlAbstractText struct {
	Label string
	Text  string
}

func (a *xmlAbstractText) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	for _, attr := range start.Attr {
		if attr.Name.Local == "Label" {
			a.Label = attr.Value
		}
	}
	s, err := collectText(d)
	a.Text = s
	return err
}

func collectText(d *xml.Decoder) (string, error) {
	var b strings.Builder
	depth := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return "", err
		}
		switch tk := tok.(type) {
		case xml.StartElement:
			depth++
		case xml.EndElement:
			if depth == 0 {
				return strings.Join(strings.Fields(b.String()), " "), nil
			}
			depth--
		case xml.CharData:
			b.Write(tk)
		}
	}
}

func (x xmlArticle) toArticle() Article {
	a := Article{
		PMID:             strings.TrimSpace(x.PMID),
		Title:            string(x.Title),
		Journal:          strings.TrimSpace(x.JournalISO),
		PublicationTypes: x.PubTypes,
	}
	if a.Journal == "" {
		a.Journal = strings.TrimSpace(x.Journal)
	}

	for _, d := range x.ArticleDates {
		if s := d.format(); s != "" {
			a.Published = s
			break
		}
	}
	if a.Published == "" {
		a.Published = x.PubDate.format()
	}
	for _, h := range x.History {
		if h.Status == "entrez" {
			a.EntrezDate = xmlDate{Year: h.Year, Month: h.Month, Day: h.Day}.format()
			break
		}
	}

	for _, id := range x.ArticleIDs {
		if id.Type == "doi" {
			a.DOI = strings.TrimSpace(id.Value)
			break
		}
	}
	if a.DOI == "" {
		for _, e := range x.ELocations {
			if e.Type == "doi" {
				a.DOI = strings.TrimSpace(e.Value)
				break
			}
		}
	}

	for _, au := range x.Authors {
		switch {
		case au.CollectiveName != "":
			a.Authors = append(a.Authors, strings.TrimSpace(au.CollectiveName))
		case au.LastName != "":
			name := strings.TrimSpace(au.LastName)
			if au.Initials != "" {
				name += " " + strings.TrimSpace(au.Initials)
			}
			a.Authors = append(a.Authors, name)
		}
	}

	// Structured abstracts arrive as labeled sections (BACKGROUND, METHODS…).
	var parts []string
	for _, t := range x.Abstract {
		if t.Text == "" {
			continue
		}
		if t.Label != "" && !strings.EqualFold(t.Label, "UNLABELLED") {
			parts = append(parts, t.Label+": "+t.Text)
		} else {
			parts = append(parts, t.Text)
		}
	}
	a.Abstract = strings.Join(parts, "\n\n")
	return a
}

var monthAbbrev = map[string]string{
	"jan": "01", "feb": "02", "mar": "03", "apr": "04", "may": "05", "jun": "06",
	"jul": "07", "aug": "08", "sep": "09", "oct": "10", "nov": "11", "dec": "12",
}

// format renders YYYY-MM-DD, YYYY-MM, or YYYY. Months come as "Oct" or "10"
// depending on the element; MedlineDate ("2020 Nov-Dec") is passed through.
func (x xmlDate) format() string {
	y := strings.TrimSpace(x.Year)
	if y == "" {
		return strings.TrimSpace(x.MedlineDate)
	}
	m := monthNumber(x.Month)
	if m == "" {
		return y
	}
	if d, err := strconv.Atoi(strings.TrimSpace(x.Day)); err == nil && d >= 1 && d <= 31 {
		return fmt.Sprintf("%s-%s-%02d", y, m, d)
	}
	return y + "-" + m
}

func monthNumber(m string) string {
	m = strings.TrimSpace(m)
	if n, err := strconv.Atoi(m); err == nil && n >= 1 && n <= 12 {
		return fmt.Sprintf("%02d", n)
	}
	if len(m) >= 3 {
		return monthAbbrev[strings.ToLower(m[:3])]
	}
	return ""
}
