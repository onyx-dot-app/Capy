package lang

import "testing"

func TestTranslationSteps(t *testing.T) {
	tests := []struct {
		name     string
		from, to string
		want     [][2]string
	}{
		{"same language", "ja-JP", "ja-JP", nil},
		{"from english", "en-US", "ja-JP", [][2]string{{"en-US", "ja-JP"}}},
		{"to english", "ja-JP", "en-US", [][2]string{{"ja-JP", "en-US"}}},
		{"bridged through english", "zh-CN", "ja-JP", [][2]string{{"zh-CN", "en-US"}, {"en-US", "ja-JP"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TranslationSteps(tt.from, tt.to)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("got %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func codes(ls []Language) map[string]bool {
	m := make(map[string]bool, len(ls))
	for _, l := range ls {
		m[l.Code] = true
	}
	return m
}

func TestSpokenAndTargets(t *testing.T) {
	spoken, detectable, targets := Spoken(), Detectable(), Targets()
	if spoken[0].Code != Auto || detectable[0].Code != "en-US" {
		t.Errorf("Spoken()[0] = %s, Detectable()[0] = %s, want auto and en-US first", spoken[0].Code, detectable[0].Code)
	}
	if !IsSpoken(Auto) || IsTarget(Auto) || codes(detectable)[Auto] {
		t.Error("auto must be spoken only, and not among the languages it detects")
	}
	sc, tc := codes(detectable), codes(targets)
	for _, code := range []string{"ko-KR", "zh-TW"} {
		if sc[code] {
			t.Errorf("Spoken() contains %s, which Scribe cannot transcribe", code)
		}
		if !tc[code] {
			t.Errorf("Targets() lacks %s", code)
		}
	}
	for code := range sc {
		if !tc[code] {
			t.Errorf("spoken language %s is not a translation target", code)
		}
	}
}

func TestReturnedSlicesAreCopies(t *testing.T) {
	Spoken()[1].Name = "mutated"
	Targets()[0].Name = "mutated"
	if l, _ := Lookup("en-US"); l.Name != "English" {
		t.Errorf("Lookup after mutation = %q", l.Name)
	}
	if Spoken()[1].Name != "English" || Targets()[0].Name != "English" {
		t.Error("mutating a returned slice leaked into package state")
	}
}

func TestFromDetected(t *testing.T) {
	tests := []struct {
		detected, want string
		ok             bool
	}{
		// Scribe's continuous detection names languages by ISO 639-1.
		{"en", "en-US", true},
		{"zh", "zh-CN", true}, // the first Chinese of the catalog
		{"ja", "ja-JP", true},
		{"pt", "pt-BR", true},
		{"ZH", "zh-CN", true},
		{"zh-TW", "zh-TW", true}, // a full code matches itself, not the first zh
		{"zh-HK", "zh-CN", true},
		{"sw", "", false},
		{"", "", false},
		{"-", "", false},
	}
	for _, tt := range tests {
		got, ok := FromDetected(tt.detected)
		if got != tt.want || ok != tt.ok {
			t.Errorf("FromDetected(%q) = %q, %v; want %q, %v", tt.detected, got, ok, tt.want, tt.ok)
		}
	}
}
