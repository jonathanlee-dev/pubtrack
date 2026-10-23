package pubmed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const searchMax = 500

type SearchResult struct {
	Count            int
	IDs              []string
	QueryTranslation string
	// PhrasesNotFound lists query terms PubMed silently dropped, usually a typo.
	PhrasesNotFound []string
}

// Search returns PMIDs matching term that were added to PubMed (Entrez date)
// in the last days days. Entrez date, not publication date, so papers that
// are indexed late or published online ahead of print aren't missed.
func (c *Client) Search(ctx context.Context, term string, days int) (SearchResult, error) {
	body, err := c.get(ctx, "esearch.fcgi", url.Values{
		"db":       {"pubmed"},
		"term":     {term},
		"datetype": {"edat"},
		"reldate":  {strconv.Itoa(days)},
		"retmax":   {strconv.Itoa(searchMax)},
		"retmode":  {"json"},
	})
	if err != nil {
		return SearchResult{}, err
	}
	return ParseSearch(body)
}

func ParseSearch(body []byte) (SearchResult, error) {
	var r struct {
		Error         string `json:"error"`
		ESearchResult struct {
			Count            string   `json:"count"`
			IDList           []string `json:"idlist"`
			QueryTranslation string   `json:"querytranslation"`
			Error            string   `json:"ERROR"`
			ErrorList        struct {
				PhrasesNotFound []string `json:"phrasesnotfound"`
			} `json:"errorlist"`
		} `json:"esearchresult"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return SearchResult{}, fmt.Errorf("esearch: decoding response: %w", err)
	}
	if r.Error != "" {
		return SearchResult{}, fmt.Errorf("esearch: %s", r.Error)
	}
	es := r.ESearchResult
	if es.Error != "" {
		return SearchResult{}, fmt.Errorf("esearch: %s", es.Error)
	}
	if strings.TrimSpace(es.Count) == "" {
		return SearchResult{}, errors.New("esearch: response has no count")
	}
	count, err := strconv.Atoi(es.Count)
	if err != nil {
		return SearchResult{}, fmt.Errorf("esearch: bad count %q", es.Count)
	}
	return SearchResult{
		Count:            count,
		IDs:              es.IDList,
		QueryTranslation: es.QueryTranslation,
		PhrasesNotFound:  es.ErrorList.PhrasesNotFound,
	}, nil
}

// CitedBy returns, for each paper citing any of pmids, which of pmids it
// cites. Citation links come from PMC full text, so coverage is partial.
func (c *Client) CitedBy(ctx context.Context, pmids []string) (map[string][]string, error) {
	// Repeated id= parameters (not a comma list) return one linkset per
	// PMID, which is what lets us tell which paper is being cited.
	body, err := c.get(ctx, "elink.fcgi", url.Values{
		"dbfrom":   {"pubmed"},
		"linkname": {"pubmed_pubmed_citedin"},
		"retmode":  {"json"},
		"id":       pmids,
	})
	if err != nil {
		return nil, err
	}
	return ParseCitedBy(body)
}

func ParseCitedBy(body []byte) (map[string][]string, error) {
	var r struct {
		Error    string `json:"ERROR"`
		LinkSets []struct {
			IDs        []string `json:"ids"`
			LinkSetDBs []struct {
				LinkName string   `json:"linkname"`
				Links    []string `json:"links"`
			} `json:"linksetdbs"`
		} `json:"linksets"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("elink: decoding response: %w", err)
	}
	if r.Error != "" {
		return nil, fmt.Errorf("elink: %s", r.Error)
	}
	out := map[string][]string{}
	for _, ls := range r.LinkSets {
		for _, cited := range ls.IDs {
			for _, db := range ls.LinkSetDBs {
				if db.LinkName != "pubmed_pubmed_citedin" {
					continue
				}
				for _, citing := range db.Links {
					out[citing] = append(out[citing], cited)
				}
			}
		}
	}
	return out, nil
}
