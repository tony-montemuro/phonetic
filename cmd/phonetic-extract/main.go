// Command phonetic-extract runs phonetic's fetch and extract steps on a URL
// and prints the result, for trying out extraction and capturing test
// fixtures.
//
//	phonetic-extract https://example.com/post
//	phonetic-extract -save internal/extract/testdata/articles/example -license "CC BY 4.0" https://example.com/post
//	phonetic-extract -save internal/extract/testdata/articles-local/mysite https://mysite.example/post   (gitignored)
//
// With -save, it writes page.html (the fetched page, as UTF-8), meta.json
// (source URL, final URL, license, capture time) and expected.json (the
// extraction, the golden file the extract tests compare against).
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/tony-montemuro/phonetic/internal/extract"
	"github.com/tony-montemuro/phonetic/internal/extract/extracttest"
	"github.com/tony-montemuro/phonetic/internal/fetch"
	"github.com/tony-montemuro/phonetic/internal/urlnorm"
)

func main() {
	save := flag.String("save", "", "directory to write page.html, meta.json and expected.json into")
	license := flag.String("license", "", "license of the page's content (recorded in meta.json)")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: phonetic-extract [-save DIR] [-license TEXT] URL")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(flag.Arg(0), *save, *license); err != nil {
		fmt.Fprintln(os.Stderr, "phonetic-extract:", err)
		os.Exit(1)
	}
}

func run(rawURL, saveDir, license string) error {
	src, err := urlnorm.Normalize(rawURL)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	res, err := fetch.New(fetch.Options{}).Fetch(ctx, src)
	if err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	final, err := url.Parse(res.URL)
	if err != nil {
		return err
	}
	a, err := extract.Extract(res.Body, final)
	if err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	if saveDir == "" {
		out, err := json.Marshal(a, jsontext.WithIndent("  "))
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}

	meta := extracttest.Meta{
		SourceURL:  src,
		FinalURL:   res.URL,
		License:    license,
		CapturedAt: res.FetchedAt.UTC().Truncate(time.Second),
	}
	if err := extracttest.Save(saveDir, res.Body, meta, a); err != nil {
		return err
	}
	fmt.Printf("saved %q: %d blocks, %d words, %d images\n", a.Title, len(a.Blocks), a.WordCount, len(a.Images))
	return nil
}
