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
			want: config{addr: ":8080", inventoryURL: "http://localhost:8081", inventoryTimeout: 2 * time.Second},
		},
		{
			name: "overrides",
			env:  map[string]string{"ADDR": ":9000", "INVENTORY_URL": "http://inventory-api:8081", "INVENTORY_TIMEOUT": "500ms"},
			want: config{addr: ":9000", inventoryURL: "http://inventory-api:8081", inventoryTimeout: 500 * time.Millisecond},
		},
		{name: "relative url", env: map[string]string{"INVENTORY_URL": "inventory-api:8081"}, wantErr: true},
		{name: "unsupported scheme", env: map[string]string{"INVENTORY_URL": "file:///etc/passwd"}, wantErr: true},
		{name: "bad timeout", env: map[string]string{"INVENTORY_TIMEOUT": "soon"}, wantErr: true},
		{name: "zero timeout", env: map[string]string{"INVENTORY_TIMEOUT": "0s"}, wantErr: true},
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
