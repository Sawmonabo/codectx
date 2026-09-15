package model

import (
	"strconv"
	"testing"
)

// TestReservedCapabilityDetailsSurviveAFullMap protects the rule that the
// report fold's own bookkeeping is never the entry evicted to make room for a
// provider detail.
//
// Before this rule, WithDetail returned the receiver unchanged once the map
// held MaxCapabilityDetails entries, so a provider that filled the map took the
// fold's `scopes` count with it: the row published no count and the reader
// under-counted the scopes it stood for, with nothing on the row saying so.
func TestReservedCapabilityDetailsSurviveAFullMap(t *testing.T) {
	c := CapabilityState{ProviderID: "p", Capability: "c", Scope: "s", State: CapabilityPartial}
	// Fill the provider budget exactly, then overflow it by three.
	for i := range MaxProviderCapabilityDetails + 3 {
		c = c.WithDetail("provider_key_"+strconv.Itoa(i), "v")
	}
	if got := c.providerDetails(); got != MaxProviderCapabilityDetails {
		t.Fatalf("the provider contributed %d details, want the %d-entry budget", got, MaxProviderCapabilityDetails)
	}
	if got := c.Details[DetailDetailsOmitted]; got != "3" {
		t.Fatalf("details_omitted is %q, want %q: a dropped provider detail must be counted", got, "3")
	}
	// The fold's keys still land on the full map.
	c = c.WithDetail(DetailScopes, "7").WithDetail(DetailUnitsFailed, "2").WithDetail(DetailDetailsTruncated, "reason")
	for key, want := range map[string]string{DetailScopes: "7", DetailUnitsFailed: "2", DetailDetailsTruncated: "reason"} {
		if got := c.Details[key]; got != want {
			t.Fatalf("reserved detail %s is %q, want %q: the fold's bookkeeping was evicted", key, got, want)
		}
	}
	if len(c.Details) > MaxCapabilityDetails {
		t.Fatalf("the detail map holds %d entries, over the %d bound", len(c.Details), MaxCapabilityDetails)
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("a row at the detail bound does not validate: %v", err)
	}
}
