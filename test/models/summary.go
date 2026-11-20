package models

// Summary is the smoke-summary interchange format consumed by modelversions.
type Summary struct {
	Results []Run `json:"results"`
}

// Run describes one smoke result at UTC day precision.
type Run struct {
	Date   string `json:"date"`
	Route  string `json:"route"`
	Model  string `json:"model"`
	Result string `json:"result"`
}
