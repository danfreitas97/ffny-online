package relay

import "testing"

func TestHelloCarriesTheBuildNumber(t *testing.T) {
	for _, h := range []Hello{
		{Mode: ModeLobby, Name: "Ann"},                    // a build before 26: no number
		{Mode: ModeLobby, Name: "Ann", BuildNo: 26},       // since 26
		{Mode: ModeLobby, Name: "Ben Ben", BuildNo: 1234}, // any name length
	} {
		raw, err := Encode(h)
		if err != nil {
			t.Fatal(err)
		}
		m, err := DecodeClient(raw)
		if err != nil {
			t.Fatalf("%+v: %v", h, err)
		}
		got := m.(Hello)
		if got.Name != h.Name || got.BuildNo != h.BuildNo {
			t.Fatalf("sent %+v, got %+v", h, got)
		}
	}
	// a name length that fits neither form is still malformed
	raw, _ := Encode(Hello{Mode: ModeLobby, Name: "Ann"})
	if _, err := DecodeClient(append(raw, 1, 2)); err == nil {
		t.Fatal("two stray bytes after the name were accepted")
	}
}

func TestOutdatedBuilds(t *testing.T) {
	published := uint32(0)
	r := &Relay{cfg: Config{PublishedBuild: func() uint32 { return published }}}
	cases := []struct {
		published, client uint32
		outdated          bool
	}{
		{0, 0, false},   // nothing published yet: everyone plays
		{25, 0, false},  // build 25 published: builds before 26 (no number) still play
		{25, 26, false}, // a newer client than the published one (a test build)
		{26, 0, true},   // 26 out: the older builds stop
		{26, 26, false},
		{27, 26, true},
		{27, 28, false},
	}
	for _, c := range cases {
		published = c.published
		if got := r.outdated(c.client); got != c.outdated {
			t.Errorf("published %d, client %d: outdated %v", c.published, c.client, got)
		}
	}
	if (&Relay{}).outdated(1) {
		t.Error("no PublishedBuild: no check")
	}
}
