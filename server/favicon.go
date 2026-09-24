package server

import "net/http"

// faviconSVG is the product mark. Without it every page load asks for
// /favicon.ico and logs a 404.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="#2A8CF0" stroke-width="2"><path d="M8.2 2.5h7.6l5.7 5.7v7.6l-5.7 5.7H8.2l-5.7-5.7V8.2z"/><circle cx="12" cy="12" r="3" fill="#2A8CF0" stroke="none"/></svg>`

func serveFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(faviconSVG))
}
