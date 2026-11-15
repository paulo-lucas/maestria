package cloud

import "testing"

func TestParseSender(t *testing.T) {
	for _, tc := range []struct {
		in, envelope, header string
	}{
		{"no-reply@example.com", "no-reply@example.com", "<no-reply@example.com>"},
		{"Maestria <no-reply@example.com>", "no-reply@example.com", `"Maestria" <no-reply@example.com>`},
		{"  Maestria <no-reply@example.com>  ", "no-reply@example.com", `"Maestria" <no-reply@example.com>`},
	} {
		env, hdr, err := parseSender(tc.in)
		if err != nil || env != tc.envelope || hdr != tc.header {
			t.Errorf("parseSender(%q) = %q, %q, %v; want %q, %q", tc.in, env, hdr, err, tc.envelope, tc.header)
		}
	}
	for _, bad := range []string{"", "not an address", "a@b.com\r\nBcc: x@y.com", "Maestria <>"} {
		if _, _, err := parseSender(bad); err == nil {
			t.Errorf("parseSender(%q) should fail", bad)
		}
	}
}

func TestValidateRejectsBadSMTPFrom(t *testing.T) {
	c := Defaults()
	c.SMTP.Host = "smtp.example.com"
	c.SMTP.From = "not an address"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate should reject an invalid smtp.from")
	}
	c.SMTP.From = "Maestria <no-reply@example.com>"
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
