package main

import "testing"

func TestWFName(t *testing.T) {
	t.Logf("name: %q", wfName("ci", "qubitdigital", "yak", "mytests/tester"))
}
