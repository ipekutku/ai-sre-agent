package main

import (
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    config
		wantErr bool
	}{
		{
			name: "defaults",
			env:  map[string]string{},
			want: config{addr: ":8081", adminAddr: "127.0.0.1:9081", baseLatency: 20 * time.Millisecond},
		},
		{
			name: "overrides",
			env:  map[string]string{"ADDR": ":9000", "ADMIN_ADDR": ":9001", "BASE_LATENCY": "0s"},
			want: config{addr: ":9000", adminAddr: ":9001", baseLatency: 0},
		},
		{name: "bad latency", env: map[string]string{"BASE_LATENCY": "fast"}, wantErr: true},
		{name: "negative latency", env: map[string]string{"BASE_LATENCY": "-1ms"}, wantErr: true},
		{name: "latency above max", env: map[string]string{"BASE_LATENCY": "1m"}, wantErr: true},
		{name: "same addresses", env: map[string]string{"ADDR": ":9000", "ADMIN_ADDR": ":9000"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := loadConfig(func(k string) string { return tt.env[k] })
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got config %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("loadConfig: %v", err)
			}
			if got != tt.want {
				t.Errorf("config = %+v, want %+v", got, tt.want)
			}
		})
	}
}
