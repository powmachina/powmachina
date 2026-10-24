package explorer

import "testing"

// Two nodes' maps add up per country; a node with the map off adds nothing.
func TestMergeGeo(t *testing.T) {
	a := LiveGeo{Enabled: true, Minutes: 30, Unknown: 2, Countries: []LiveCountry{{"DE", 3, 10}, {"SK", 1, 4}}}
	b := LiveGeo{Enabled: true, Minutes: 30, Unknown: 1, Countries: []LiveCountry{{"SK", 5, 6}, {"US", 2, 2}}}
	got := mergeGeo(a, b)
	if got.Unknown != 3 || len(got.Countries) != 3 {
		t.Fatalf("%+v", got)
	}
	if got.Countries[0] != (LiveCountry{"SK", 6, 10}) || got.Countries[1] != (LiveCountry{"DE", 3, 10}) {
		t.Fatalf("order or sums: %+v", got.Countries)
	}
	if off := mergeGeo(got, LiveGeo{Countries: []LiveCountry{{"FR", 9, 9}}}); len(off.Countries) != 3 {
		t.Fatal("a node with the map off was counted")
	}
	if on := mergeGeo(LiveGeo{}, b); !on.Enabled || on.Minutes != 30 {
		t.Fatal("the main node without a map should take the other's")
	}
}
