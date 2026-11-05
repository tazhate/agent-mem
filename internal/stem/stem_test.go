package stem

import "testing"

func TestRussianForms(t *testing.T) {
	if Word("запечатан") != Word("запечатанный") {
		t.Fatalf("%q != %q", Word("запечатан"), Word("запечатанный"))
	}
	if Word("running") != Word("runs") && Word("running") != "run" {
		t.Fatalf("english stem: %q", Word("running"))
	}
	if Word("k8s") != "k8s" {
		t.Fatal("identifiers must stay")
	}
}

func TestQuery(t *testing.T) {
	q := Query("почему Vault запечатан и что делать")
	if len(q) == 0 || q[0] != Word("почему") && q[0] != "vault" {
		t.Logf("query: %v", q)
	}
	for _, s := range q {
		if s == "и" || s == "что" {
			t.Fatalf("stop word kept: %v", q)
		}
	}
}
