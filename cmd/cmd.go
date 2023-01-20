package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/bmaupin/go-epub"
	"github.com/chromedp/cdproto/network"
	"github.com/jtagcat/util/scrape"
	"github.com/urfave/cli/v2"
)

var App = &cli.App{
	Name:      "ellu-dl",
	Usage:     "Utility to convert Ellu reader to EPUB",
	ArgsUsage: "<url>",
	Flags: []cli.Flag{
		&cli.StringFlag{Name: "cookie", EnvVars: []string{"ELLU_COOKIE"}, Usage: "\"sid\" cookie", Required: true},
		&cli.BoolFlag{Name: "preview", Usage: "consume /reader-preview instead of /reader"},
	},
	Before: func(ctx *cli.Context) error {
		want := 1
		if ctx.Args().Len() != want {
			return fmt.Errorf("expected %d args, got %d", want, ctx.Args().Len())
		}
		return nil
	},
	Action: func(ctx *cli.Context) error {
		pUrl, err := url.Parse(ctx.Args().Get(0))
		if err != nil {
			return err
		}

		c := scrape.InitScraper(ctx.Context, &scrape.Scraper{
			Cookies: []*network.CookieParam{{
				Name:   "sid",
				Value:  ctx.String("cookie"),
				Domain: pUrl.Host,
			}},
			Timeout: 30 * time.Second,
		})

		book, err := getMetadata(c, pUrl)
		if err != nil {
			return fmt.Errorf("getting book id: %w", err)
		}

		root := pUrl.Scheme + "://" + pUrl.Host

		previewStr := ""
		if ctx.Bool("preview") {
			previewStr = "-preview"
		}
		rootURL := fmt.Sprintf("%s/reader%s?book_id=%d", root, previewStr, book.Id)

		if err = book.getChapters(c, rootURL); err != nil {
			return fmt.Errorf("getting book metadata: %w", err)
		}

		err = book.chaptersPopulate(c, rootURL)
		if err != nil {
			return fmt.Errorf("populating book chapters: %w", err)
		}

		// Create a new EPUB
		e := epub.NewEpub(book.Title)
		e.SetIdentifier(strconv.Itoa(book.ISBN))
		e.SetAuthor(book.Author)

		err = book.filesPopulate(c, root, e)
		if err != nil {
			return fmt.Errorf("populating EPUB files: %w", err)
		}

		for _, chapter := range book.Chapters {
			_, err := e.AddSection(chapter.Content, chapter.Title, "", "")
			if err != nil {
				return fmt.Errorf("adding chapter %d to epub: %w", chapter.Id, err)
			}
		}

		err = e.Write(fmt.Sprintf("%s (%d).epub", book.Title, book.Id))
		return err
	},
}

type (
	Book struct {
		Title    string
		Id       int
		ISBN     int
		Author   string
		Chapters []Chapter
	}
	Chapter struct {
		Title   string
		Id      int    `json:"number"`
		Content string // html
	}
)

func getMetadata(c *scrape.Scraper, pUrl *url.URL) (b Book, err error) {
	if !strings.HasPrefix(pUrl.Path, "/books/") {
		return b, fmt.Errorf("URL format not recognized (expected /books/)")
	}

	isbnS := strings.TrimPrefix(pUrl.Path, "/books/")
	isbnS, _, _ = strings.Cut(isbnS, "/")
	b.ISBN, err = strconv.Atoi(isbnS)
	if err != nil {
		return b, fmt.Errorf("converting ISBN: %w", err)
	}

	url := pUrl.String()

	s, _, err := c.Get(url, "*")
	if err != nil {
		return b, err
	}

	for _, node := range scrape.RawEach(s.Find("#book_id")) {
		idS, ok := node.Attr("value")
		if !ok {
			return b, fmt.Errorf("book_id not found")
		}
		b.Id, err = strconv.Atoi(idS)
		if err != nil {
			return b, fmt.Errorf("converting book_id: %w", err)
		}
	}

	s = s.Find(".book-head")
	b.Title = s.ChildrenFiltered("h1").Text()
	b.Author = s.ChildrenFiltered("p").Text()
	b.Author = strings.TrimSpace(b.Author)

	return b, nil
}

func (book *Book) getChapters(c *scrape.Scraper, rootURL string) error {
	s, _, err := c.Get(rootURL, "*")
	if err != nil {
		return err
	}

	var found bool

	script := s.Find("body > script").Text()
	for _, line := range strings.Split(script, "\n") {

		if !strings.HasPrefix(line, "new Reader(") {
			continue
		}
		found = true

		// JS arguments, so CSV with various quotes and sometimes no quotes

		chapInfo := strings.Split(line, ", ")[3:] // ignore totalChapterBytes, and bookmark
		chapInfo = chapInfo[:len(chapInfo)-3]     // remove JS stuff
		chapJson := strings.Join(chapInfo, ", ")

		err = json.Unmarshal([]byte(chapJson), &book.Chapters)
		if err != nil {
			return fmt.Errorf("unmarshalling chapters info")
		}

	}

	if !found {
		return fmt.Errorf("chapters info not found")
	}

	return nil
}

func errIgnore[T any](a T, _ error) T {
	return a
}

func (b *Book) chaptersPopulate(c *scrape.Scraper, rootURL string) error {
	for i := range b.Chapters {
		chap := &b.Chapters[i]

		chapurl := rootURL + "&chapter_number=" + strconv.Itoa(chap.Id)
		body, err := c.DoRaw(chapurl, http.MethodPost, nil)
		if err != nil {
			return fmt.Errorf("chapter %d content with POST: %w", chap.Id, err)
		}

		var chapWrap struct {
			Chapter string // HTML
		}
		if err := json.Unmarshal(body, &chapWrap); err != nil {
			return fmt.Errorf("chapter %d content unmarshaling: %w", chap.Id, err)
		}

		chap.Content = string(chapWrap.Chapter)
	}

	return nil
}

func (b *Book) filesPopulate(c *scrape.Scraper, rootPath string, e *epub.Epub) (_ error) {
	for i := range b.Chapters {
		chap := &b.Chapters[i]

		s, err := goquery.NewDocumentFromReader(strings.NewReader(chap.Content))
		if err != nil {
			return fmt.Errorf("chapter %d initializing goquery: %w", chap.Id, err)
		}

		for _, elem := range scrape.RawEach(s.Find("*")) {
			for i := range elem.Nodes[0].Attr {
				attr := &elem.Nodes[0].Attr[i]

				if strings.HasPrefix(attr.Val, "/static/") {

					urlS, err := url.JoinPath(rootPath, attr.Val)
					if err != nil {
						return fmt.Errorf("chapter %d: %w", chap.Id, err)
					}

					attr.Val, err = e.AddImage(urlS, "")
					if err != nil {
						return fmt.Errorf("chapter %d, adding image to EPUB: %w", chap.Id, err)
					}
					if chap.Id == 0 {
						e.SetCover(attr.Val, "")
					}
				}
			}
		}

		chap.Content, err = s.Html()
		if err != nil {
			return fmt.Errorf("chapter %d rendering substituted HTML: %w", chap.Id, err)
		}
	}

	return nil
}
