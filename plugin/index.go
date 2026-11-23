package plugin

// Index is what a plugin source serves: the bundles it offers.
type Index struct {
	Plugins []IndexEntry `json:"plugins"`
}

// IndexEntry is one bundle a source lists. URL is resolved against the
// index's own URL and must stay under its folder; SHA256 is the manifest's
// digest, which the bundle fetched must have.
type IndexEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	URL         string `json:"url"`
	SHA256      string `json:"sha256"`
	Publisher   string `json:"publisher,omitempty"`
	Description string `json:"description,omitempty"`
	// Signer is who signed it, when signed, as the source checked it.
	Signer string `json:"signer,omitempty"`
}
