package ws

import "testing"

func TestParseNotification(t *testing.T) {
	tests := []struct {
		payload string
		want    Notification
	}{
		{
			payload: "machines:abc-123",
			want:    Notification{Table: "machines", ID: "abc-123"},
		},
		{
			payload: "ssh_keys:deleted:abc-123",
			want:    Notification{Table: "ssh_keys", ID: "abc-123", Deleted: true},
		},
		{
			payload: "deploy_logs:machine-uuid",
			want:    Notification{Table: "deploy_logs", ID: "machine-uuid"},
		},
		{
			payload: "operations:550e8400-e29b-41d4-a716-446655440000",
			want:    Notification{Table: "operations", ID: "550e8400-e29b-41d4-a716-446655440000"},
		},
		{
			payload: "images:deleted:550e8400-e29b-41d4-a716-446655440000",
			want:    Notification{Table: "images", ID: "550e8400-e29b-41d4-a716-446655440000", Deleted: true},
		},
		{
			payload: "",
			want:    Notification{},
		},
		{
			payload: "garbage",
			want:    Notification{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.payload, func(t *testing.T) {
			got := ParseNotification(tt.payload)
			if got.Table != tt.want.Table {
				t.Errorf("Table: got %q, want %q", got.Table, tt.want.Table)
			}
			if got.ID != tt.want.ID {
				t.Errorf("ID: got %q, want %q", got.ID, tt.want.ID)
			}
			if got.Deleted != tt.want.Deleted {
				t.Errorf("Deleted: got %v, want %v", got.Deleted, tt.want.Deleted)
			}
		})
	}
}
