package auth

import "testing"

func TestStateOK(t *testing.T) {
	const want = "s3cret"
	cases := []struct {
		name string
		res  callbackResult
		ok   bool
	}{
		{"http matching state", callbackResult{state: want}, true},
		{"http empty state rejected (CSRF)", callbackResult{state: ""}, false},
		{"http wrong state rejected", callbackResult{state: "other"}, false},
		{"paste bare code (no state) trusted", callbackResult{state: "", viaPaste: true}, true},
		{"paste url matching state", callbackResult{state: want, viaPaste: true}, true},
		{"paste url wrong state rejected", callbackResult{state: "other", viaPaste: true}, false},
	}
	for _, c := range cases {
		if got := stateOK(c.res, want); got != c.ok {
			t.Errorf("%s: stateOK=%v, want %v", c.name, got, c.ok)
		}
	}
}
