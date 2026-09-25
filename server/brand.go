package server

import (
	"embed"
	"encoding/base64"
	"strings"
)

// The brand images travel inside the pages as data URIs: the pages load
// nothing from anywhere, and no route outside the auth gate is added for them.
//
//go:embed brand/*
var brandFS embed.FS

func brandURI(name, mime string) string {
	b, err := brandFS.ReadFile("brand/" + name)
	if err != nil {
		panic("server: missing embedded brand asset " + name)
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

var brandIcon, _ = brandFS.ReadFile("brand/icon.png")

// brandify fills a page template's brand placeholders: the lockup and its
// CSS first, then the images they refer to.
func brandify(page string) string {
	page = strings.NewReplacer(
		"{{BRAND_LOCKUP}}", brandLockup,
		"/*{{BRAND_CSS}}*/", brandCSS,
	).Replace(page)
	return strings.NewReplacer(
		"{{BRAND_MARK}}", brandURI("mark.webp", "image/webp"),
		"{{BRAND_MARK_REV}}", brandURI("mark-rev.webp", "image/webp"),
		"{{BRAND_WORD}}", brandURI("word.webp", "image/webp"),
		"{{BRAND_WORD_REV}}", brandURI("word-rev.webp", "image/webp"),
		"{{BRAND_HERO}}", brandURI("hero.webp", "image/webp"),
		"{{BRAND_HERO_REV}}", brandURI("hero-rev.webp", "image/webp"),
		"{{BRAND_ICON}}", brandURI("icon.png", "image/png"),
	).Replace(page)
}

// brandLockup is the header logo; brandCSS shows the variant for the theme.
const brandLockup = `<span class="lockup"><img class="lk-light" src="{{BRAND_MARK}}" alt="" width="34" height="32"><img class="lk-light lk-word" src="{{BRAND_WORD}}" alt="Abhed" width="95" height="22"><img class="lk-dark" src="{{BRAND_MARK_REV}}" alt="" width="34" height="32"><img class="lk-dark lk-word" src="{{BRAND_WORD_REV}}" alt="Abhed" width="95" height="22"></span>`

// brandCSS follows the same theme selectors as the pages' palettes.
const brandCSS = `:root{--on-accent:#fff}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){--on-accent:#0B0B0C}}
:root[data-theme="dark"]{--on-accent:#0B0B0C}
.lockup{display:inline-flex;align-items:center;gap:8px}
.lockup img{display:block;height:24px;width:auto}
.lockup .lk-word{height:17px}
.lk-dark{display:none!important}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]) .lk-light{display:none!important}:root:not([data-theme="light"]) .lk-dark{display:block!important}}
:root[data-theme="dark"] .lk-light{display:none!important}
:root[data-theme="dark"] .lk-dark{display:block!important}
`
