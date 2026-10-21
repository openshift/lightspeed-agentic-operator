// Package metrics contains isolated tests for the deployed operator's metrics.
package metrics

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

func positiveQueryResult(body []byte) error {
	var response struct {
		Status string `json:"status"`
		Error  string `json:"error"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Value []json.RawMessage `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return fmt.Errorf("decode Prometheus response: %w", err)
	}
	if response.Status != "success" || response.Data.ResultType != "vector" || len(response.Data.Result) != 1 || len(response.Data.Result[0].Value) != 2 {
		return fmt.Errorf("expected one Prometheus vector sample: status=%q error=%q results=%d", response.Status, response.Error, len(response.Data.Result))
	}
	var raw string
	if err := json.Unmarshal(response.Data.Result[0].Value[1], &raw); err != nil {
		return err
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return fmt.Errorf("expected positive finite sample, got %q", raw)
	}
	return nil
}
