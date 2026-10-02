// Package ingest はフォルダスキャン・DB統合の結果を fsdb.Builder に流し込む。
package ingest

// Result は取込/統合の結果概要。
type Result struct {
	Items    int64    `json:"items"`
	Errors   int64    `json:"errors"`
	Warnings []string `json:"warnings"`
}
