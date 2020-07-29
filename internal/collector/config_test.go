package collector

import (
	"github.com/stretchr/testify/assert"
	"testing"
)

func TestNewPostgresServiceConfig(t *testing.T) {
	var testCases = []struct {
		name    string
		connStr string
		valid   bool
	}{
		{name: "valid config", connStr: "host=postgres dbname=postgres user=postgres", valid: true},
		{name: "invalid config", connStr: "invalid", valid: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewPostgresServiceConfig(tc.connStr)
			if tc.valid {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}
