package types

import "testing"

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{in: "20G", want: 20 << 30},
		{in: "20Gi", want: 20 << 30},
		{in: "20GiB", want: 20 << 30},
		{in: "20gi", want: 20 << 30},
		{in: " 512Mi ", want: 512 << 20},
		{in: "1024", want: 1024},
		{in: "20i", wantErr: true},
		{in: "", wantErr: true},
		{in: "abc", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseSize(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseSize(%q) err=%v wantErr=%v", tt.in, err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}
