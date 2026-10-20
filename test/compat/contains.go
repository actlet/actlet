package compat

import (
	"encoding/json"
	"fmt"
	"reflect"
)

// Contains reports whether the JSON document got holds everything the
// JSON document want does: the same values, in objects that may have
// fields want hasn't. A format may gain fields compatibly; it may not
// lose or change them.
func Contains(want, got []byte) error {
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		return fmt.Errorf("want: %w", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		return fmt.Errorf("got: %w", err)
	}
	return contains("$", w, g)
}

func contains(at string, want, got any) error {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: an object became %T", at, got)
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("%s.%s is gone", at, k)
			}
			if err := contains(at+"."+k, wv, gv); err != nil {
				return err
			}
		}
		return nil
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Errorf("%s: %d elements became %v", at, len(w), got)
		}
		for i := range w {
			if err := contains(fmt.Sprintf("%s[%d]", at, i), w[i], g[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if !reflect.DeepEqual(want, got) {
		return fmt.Errorf("%s: %v became %v", at, want, got)
	}
	return nil
}
