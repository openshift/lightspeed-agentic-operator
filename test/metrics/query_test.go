package metrics

import "testing"

func TestPositiveQueryResult(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"healthy", `{"status":"success","data":{"resultType":"vector","result":[{"value":[123,"1"]}]}}`, true},
		{"down", `{"status":"success","data":{"resultType":"vector","result":[{"value":[123,"0"]}]}}`, false},
		{"missing", `{"status":"success","data":{"resultType":"vector","result":[]}}`, false},
		{"query error", `{"status":"error","error":"bad query"}`, false},
		{"malformed", `{}`, false},
		{"NaN", `{"status":"success","data":{"resultType":"vector","result":[{"value":[123,"NaN"]}]}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := positiveQueryResult([]byte(tc.body)); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, error=%v", tc.valid, err)
			}
		})
	}
}
