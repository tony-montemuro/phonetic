# Extraction fixtures

Captured pages replayed by `TestFixtures` (`internal/extract/golden_test.go`).
Each directory holds `page.html` (the page as fetched, converted to UTF-8),
`meta.json` (source URL, final URL, license, capture time) and
`expected.json` (the golden extraction).

Only openly licensed or public-domain pages are committed here; the license
is recorded in each `meta.json` (and `TestFixtures` fails if it's missing).
Pages from other sites go in the gitignored `../articles-local/` directory,
which `TestFixtures` also replays when present.

## Licensing

These files are third-party content, not covered by phonetic's own license.
Each `page.html` is the page as published, converted to UTF-8; the only other
change is that `mozilla-hacks-wasi` has its reader-comments section removed
(third-party content and personal names). Each `expected.json` is derived from
its page (an extraction of its text) and is offered under the same license as
that page. Attribution is given below; for share-alike licenses, the same
license applies to these derived files.

Capture a new fixture:

    go run ./cmd/phonetic-extract -save internal/extract/testdata/articles/NAME -license "LICENSE" URL
    go run ./cmd/phonetic-extract -save internal/extract/testdata/articles-local/NAME URL

After an intentional extraction change (or a readability upgrade), regenerate
the golden files and review the diff like code:

    go test ./internal/extract -run TestFixtures -update

| Fixture | Source (see `meta.json` for the exact URL) | License |
|---|---|---|
| goblog-pipelines | "Go Concurrency Patterns: Pipelines and cancellation", The Go Blog | [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/), The Go Authors |
| goblog-slog | "Structured Logging with slog", The Go Blog | [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/), The Go Authors |
| govuk-ai-biosecurity | "Introducing the AI Biosecurity Lab", GOV.UK | [Open Government Licence v3.0](https://www.nationalarchives.gov.uk/doc/open-government-licence/version/3/), Crown copyright (the Royal Arms and crest are excluded) |
| mdn-http-caching | "HTTP caching", MDN Web Docs | [CC BY-SA 2.5](https://creativecommons.org/licenses/by-sa/2.5/), Mozilla Contributors |
| mozilla-hacks-wasi | "Standardizing WASI", Mozilla Hacks (Lin Clark) | [CC BY-SA 3.0](https://creativecommons.org/licenses/by-sa/3.0/), Mozilla |
| nasa-webb-3iatlas | "NASA's Webb Detects Methane on Interstellar Comet 3I/ATLAS", NASA Science | Public domain (U.S. Government work); NASA insignia not licensed |
| python-logging-howto | "Logging HOWTO", Python documentation | [PSF License / Zero-Clause BSD](https://docs.python.org/3/license.html), Python Software Foundation |
| wikipedia-euler | "Euler's identity", English Wikipedia | [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/), Wikipedia contributors |
| wikipedia-fr-balado | "Podcast" (redirected from "Baladodiffusion"), French Wikipedia | [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/), contributeurs de Wikipedia |
| wikipedia-podcast | "Podcast", English Wikipedia | [CC BY-SA 4.0](https://creativecommons.org/licenses/by-sa/4.0/), Wikipedia contributors |
