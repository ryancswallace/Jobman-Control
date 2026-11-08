package configjson

import "testing"

func TestExactTypedDocuments(t *testing.T) {
	t.Parallel()
	for _, input := range []string{`{"items":[{"name":"ok"}]}`, `{"items":[]}`} {
		var document struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		if err := Decode([]byte(input), &document); err != nil {
			t.Fatal(err)
		}
	}
	for _, input := range []string{`{"items":null}`, `{"Items":[]}`, `{"items":[{"Name":"x"}]}`, `{"items":[],"items":[]}`, `{"items":[]} {}`, `{"items":[{"name":"x","unknown":1}]}`} {
		var document struct {
			Items []struct {
				Name string `json:"name"`
			} `json:"items"`
		}
		if err := Decode([]byte(input), &document); err == nil {
			t.Fatal("ambiguous typed document accepted")
		}
	}
}
