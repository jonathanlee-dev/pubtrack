package pubmed

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// efetch.xml is a recorded response for 40974151, 33827088, 37935416, and
// 42671995 (an erratum with no abstract). The abstract text is replaced with
// placeholders; the section labels and copyright lines are as recorded.
func TestParseArticles(t *testing.T) {
	arts, err := ParseArticles(readFixture(t, "efetch.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(arts) != 4 {
		t.Fatalf("got %d articles, want 4", len(arts))
	}
	byID := map[string]Article{}
	for _, a := range arts {
		byID[a.PMID] = a
	}

	walk := byID["40974151"]
	if !strings.HasPrefix(walk.Title, "Memories on the move") {
		t.Errorf("title = %q", walk.Title)
	}
	if walk.Journal != "Gerontologist" {
		t.Errorf("journal = %q", walk.Journal)
	}
	if walk.Published != "2025-10-22" {
		t.Errorf("published = %q", walk.Published)
	}
	if walk.EntrezDate != "2025-09-20" {
		t.Errorf("entrez date = %q", walk.EntrezDate)
	}
	if walk.DOI != "10.1093/geront/gnaf213" {
		t.Errorf("doi = %q", walk.DOI)
	}
	if len(walk.Authors) != 16 || walk.Authors[0] != "Croff R" {
		t.Errorf("authors = %v", walk.Authors)
	}
	if !strings.HasPrefix(walk.Abstract, "BACKGROUND AND OBJECTIVES: Placeholder for the background") {
		t.Errorf("abstract starts %q", walk.Abstract[:min(60, len(walk.Abstract))])
	}
	if n := strings.Count(walk.Abstract, "\n\n"); n != 3 {
		t.Errorf("abstract has %d section breaks, want 3", n)
	}
	if strings.Contains(walk.Abstract, "©") {
		t.Error("copyright line leaked into the abstract")
	}

	// Year-only PubDate; the electronic ArticleDate is more precise.
	if got := byID["33827088"].Published; got != "2021-04-07" {
		t.Errorf("SMART published = %q, want 2021-04-07", got)
	}

	// The reference list carries its own ArticleIdLists with DOIs; only the
	// paper's own DOI may be used.
	iconect := byID["37935416"]
	if iconect.DOI != "10.1093/geront/gnad147" {
		t.Errorf("I-CONECT doi = %q", iconect.DOI)
	}
	found := false
	for _, a := range iconect.Authors {
		if a == "I-CONECT Team" {
			found = true
		}
	}
	if !found {
		t.Errorf("collective author missing: %v", iconect.Authors)
	}

	erratum := byID["42671995"]
	if erratum.Abstract != "" {
		t.Errorf("erratum abstract = %q, want empty", erratum.Abstract)
	}
	if !erratum.IsErratum() {
		t.Errorf("erratum not detected; types = %v", erratum.PublicationTypes)
	}
	if byID["40974151"].IsErratum() {
		t.Error("journal article flagged as erratum")
	}
}

func TestParseArticlesInlineMarkup(t *testing.T) {
	const doc = `<?xml version="1.0"?>
<PubmedArticleSet><PubmedArticle><MedlineCitation><PMID>1</PMID><Article>
<Journal><Title>J</Title><JournalIssue><PubDate><MedlineDate>2020 Nov-Dec</MedlineDate></PubDate></JournalIssue></Journal>
<ArticleTitle>Effects of <i>APOE</i> ε4 on gait</ArticleTitle>
<Abstract><AbstractText>R<sup>2</sup> was 0.4 in <b>older</b>
   adults.</AbstractText></Abstract>
</Article></MedlineCitation></PubmedArticle></PubmedArticleSet>`
	arts, err := ParseArticles([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	a := arts[0]
	if a.Title != "Effects of APOE ε4 on gait" {
		t.Errorf("title = %q", a.Title)
	}
	if a.Abstract != "R2 was 0.4 in older adults." {
		t.Errorf("abstract = %q", a.Abstract)
	}
	if a.Published != "2020 Nov-Dec" {
		t.Errorf("published = %q", a.Published)
	}
	if a.Journal != "J" {
		t.Errorf("journal = %q", a.Journal)
	}
}

func TestParseArticlesErrorDocument(t *testing.T) {
	_, err := ParseArticles([]byte(`<eFetchResult><ERROR>Empty id list - nothing todo</ERROR></eFetchResult>`))
	if err == nil || !strings.Contains(err.Error(), "eFetchResult") {
		t.Fatalf("err = %v, want unexpected-response error", err)
	}
}

func TestParseSearch(t *testing.T) {
	res, err := ParseSearch(readFixture(t, "esearch.json"))
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 46 || len(res.IDs) != 5 || res.IDs[0] != "42798168" {
		t.Errorf("got count=%d ids=%v", res.Count, res.IDs)
	}
}

func TestParseSearchBackendError(t *testing.T) {
	_, err := ParseSearch(readFixture(t, "esearch_error.json"))
	if err == nil || !strings.Contains(err.Error(), "Search Backend failed") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseSearchPhrasesNotFound(t *testing.T) {
	body := `{"esearchresult":{"count":"2","idlist":["1","2"],"errorlist":{"phrasesnotfound":["erratum"],"fieldsnotfound":[]}}}`
	res, err := ParseSearch([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.PhrasesNotFound, []string{"erratum"}) {
		t.Errorf("phrases not found = %v", res.PhrasesNotFound)
	}
}

// elink_citedin.json is a recorded citedin response for 33827088 and 32310138.
func TestParseCitedBy(t *testing.T) {
	m, err := ParseCitedBy(readFixture(t, "elink_citedin.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(m) != 30 {
		t.Errorf("got %d citing papers, want 30 (18 + 14 minus 2 that cite both)", len(m))
	}
	if got := m["40984050"]; !reflect.DeepEqual(got, []string{"33827088", "32310138"}) {
		t.Errorf("40984050 cites %v", got)
	}
	if got := m["42258791"]; !reflect.DeepEqual(got, []string{"33827088"}) {
		t.Errorf("42258791 cites %v", got)
	}
}

func TestDedupeKeys(t *testing.T) {
	// The eval set's preprint and journal version of one paper.
	preprint := Article{
		DOI:   "10.1101/2025.11.05.686472",
		Title: "Bridging Language Markers and Pathology: Correlations Between Digital Speech Measures and Surrogate CSF Biomarkers in Alzheimer's Disease.",
	}
	journal := Article{
		DOI:   "10.1002/DAD2.70466",
		Title: "Bridging language markers and pathology: correlations between digital speech measures and surrogate CSF biomarkers in Alzheimer's disease.",
	}
	p, j := preprint.DedupeKeys(), journal.DedupeKeys()
	if len(p) != 2 || len(j) != 2 {
		t.Fatalf("keys: %v and %v, want a DOI and a title key each", p, j)
	}
	if p[1] != j[1] {
		t.Errorf("title keys differ:\n  %s\n  %s", p[1], j[1])
	}
	if p[0] == j[0] {
		t.Error("different DOIs gave the same key")
	}
	if j[0] != "doi:10.1002/dad2.70466" {
		t.Errorf("DOI key = %q, want lowercased", j[0])
	}

	short := Article{Title: "Reply to the letter."}
	if keys := short.DedupeKeys(); len(keys) != 0 {
		t.Errorf("short title gave keys %v, want none", keys)
	}
}
