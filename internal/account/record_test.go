package account

import (
	"strings"
	"testing"
	"time"
)

func TestMaskLoginName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"user@example.test", "u***@example.test"},
		{"  spaced@corp.example  ", "s***@corp.example"},
		{"ab", "a***"},
		{"a", "a***"},
		{"汉字登录@example.test", "汉***@example.test"},
		{"@domain.test", "***@domain.test"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := MaskLoginName(tc.in); got != tc.want {
			t.Fatalf("MaskLoginName(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRecordValidate(t *testing.T) {
	valid := Record{
		ID:              "account-a",
		Label:           "账号一",
		MaskedLoginName: "u***@example.test",
		Status:          StatusEnabled,
		CreatedAt:       time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	invalid := valid
	invalid.ID = ""
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted an empty ID")
	}
	invalid = valid
	invalid.ID = "Account A!"
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted an invalid ID")
	}
	invalid = valid
	invalid.Label = "  "
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted an empty label")
	}
	invalid = valid
	invalid.Status = "paused"
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted an unknown status")
	}
	invalid = valid
	invalid.RemoteUserID = -1
	if err := invalid.Validate(); err == nil {
		t.Fatal("Validate() accepted a negative remote user ID")
	}
}

func TestRecordValidateRejectsUnmaskedLoginName(t *testing.T) {
	record := Record{
		ID:              "account-a",
		Label:           "账号一",
		MaskedLoginName: "user@example.test",
		Status:          StatusEnabled,
	}
	if err := record.Validate(); err == nil {
		t.Fatal("Validate() accepted an unmasked login name")
	}
	if strings.Contains(MaskLoginName("user@example.test"), "user") {
		t.Fatal("masked login name retained the local part")
	}
}
