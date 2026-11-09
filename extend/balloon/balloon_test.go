package balloon

import "testing"

func TestParseSpecResolve(t *testing.T) {
	const mem = int64(4 << 30)
	tests := []struct {
		in       string
		want     int64
		parseErr bool
		sizeErr  bool
	}{
		{in: "50%", want: mem / 2},
		{in: "0%", want: 0},
		{in: "99%", want: 4252016640},
		{in: "30%", want: 1288486912},
		{in: "4097", want: 4096},
		{in: "100%", parseErr: true},
		{in: "-1%", parseErr: true},
		{in: "1.5%", parseErr: true},
		{in: "2G", want: 2 << 30},
		{in: "512Mi", want: 512 << 20},
		{in: "0", want: 0},
		{in: "4G", sizeErr: true},
		{in: "8G", sizeErr: true},
		{in: "lots", parseErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			spec, err := ParseSpec(tt.in)
			if (err != nil) != tt.parseErr {
				t.Fatalf("ParseSpec(%q) err = %v, want error %v", tt.in, err, tt.parseErr)
			}
			if tt.parseErr {
				return
			}
			got, err := spec.Resolve(mem)
			if (err != nil) != tt.sizeErr {
				t.Fatalf("Resolve err = %v, want error %v", err, tt.sizeErr)
			}
			if !tt.sizeErr && got != tt.want {
				t.Errorf("Resolve = %d, want %d", got, tt.want)
			}
		})
	}
}
