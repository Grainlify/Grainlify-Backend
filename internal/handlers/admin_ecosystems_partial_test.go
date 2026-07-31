package handlers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEcosystemUpdateArraysDistinguishOmittedAndEmpty(t *testing.T) {
	var omitted ecosystemUpsertRequest
	require.NoError(t, json.Unmarshal([]byte(`{"name":"rename only"}`), &omitted))
	require.Empty(t, omitted.Links)
	require.Empty(t, omitted.KeyAreas)
	require.Empty(t, omitted.Technologies)

	var cleared ecosystemUpsertRequest
	require.NoError(t, json.Unmarshal([]byte(`{"links":[],"key_areas":[],"technologies":[]}`), &cleared))
	require.Equal(t, json.RawMessage("[]"), cleared.Links)
	require.Equal(t, json.RawMessage("[]"), cleared.KeyAreas)
	require.Equal(t, json.RawMessage("[]"), cleared.Technologies)
}
