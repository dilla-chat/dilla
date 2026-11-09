//go:build !dilla_testhost

package main

// testhostTool is only a test host's: a shipped Bot finds its provider tools on its PATH.
func testhostTool(string) string { return "" }
