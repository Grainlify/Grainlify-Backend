package handlers

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUpdateMetadataTagsDistinguishesOmittedAndEmpty(t *testing.T) {
	var omitted updateMetadataRequest
	require.NoError(t, json.Unmarshal([]byte(`{"description":"keep tags"}`), &omitted))
	require.Nil(t, omitted.Tags)

	var cleared updateMetadataRequest
	require.NoError(t, json.Unmarshal([]byte(`{"tags":[]}`), &cleared))
	require.NotNil(t, cleared.Tags)
	require.Empty(t, *cleared.Tags)
}
