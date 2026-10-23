//go:build deps

// Package deps keeps the modules the slice uses in go.mod before the code that imports them
// lands. It builds only with -tags deps; delete an import here once real code imports it.
package deps

import (
	_ "github.com/PuerkitoBio/goquery"
	_ "github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/pemistahl/lingua-go"
	_ "github.com/pressly/goose/v3"
	_ "go.yaml.in/yaml/v3"
	_ "golang.org/x/net/html"
	_ "golang.org/x/net/publicsuffix"
	_ "golang.org/x/time/rate"
)
