package exec

import (
	"reflect"
	"testing"
)

func TestWrapCommandInShell(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		expected []string
	}{
		{
			name:     "empty args",
			args:     []string{},
			expected: []string{},
		},
		{
			name:     "single command",
			args:     []string{"hostname"},
			expected: []string{"sh", "-c", "hostname"},
		},
		{
			name:     "command with args",
			args:     []string{"pip", "install", "-r", "requirements.txt"},
			expected: []string{"sh", "-c", "pip install -r requirements.txt"},
		},
		{
			name:     "command with pipes as single quoted string",
			args:     []string{"apt update && apt install -y vim"},
			expected: []string{"sh", "-c", "apt update && apt install -y vim"},
		},
		{
			name:     "cd command in quoted string",
			args:     []string{"cd /app && pip install .[core,tpu]"},
			expected: []string{"sh", "-c", "cd /app && pip install .[core,tpu]"},
		},
		{
			name:     "command with pipes",
			args:     []string{"echo hello | grep hello"},
			expected: []string{"sh", "-c", "echo hello | grep hello"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := WrapCommandInShell(tt.args)
			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("WrapCommandInShell(%v) = %v, want %v", tt.args, result, tt.expected)
			}
		})
	}
}
