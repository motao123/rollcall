package theme

import "testing"

func TestValidate(t *testing.T) {
	radius := 10
	valid := []Theme{
		Default(),
		{Mode: "light", Accent: "#2F5D8A", AccentDark: "#6ea8d8", Radius: &radius, Compact: true},
		{Mode: "dark"},
		{Mode: "auto", Accent: "#000000"},
	}
	for _, th := range valid {
		if err := th.Validate(); err != nil {
			t.Fatalf("%+v 应通过校验: %v", th, err)
		}
	}

	badRadius := 25
	negative := -1
	invalid := []Theme{
		{Mode: "system"},
		{Accent: "red"},
		{Accent: "#fff"},
		{Accent: "#1234567"},
		{AccentDark: "url(x)"},
		{Radius: &badRadius},
		{Radius: &negative},
	}
	for _, th := range invalid {
		if err := th.Validate(); err == nil {
			t.Fatalf("%+v 应被拒绝", th)
		}
	}
}

func TestParseAndMarshal(t *testing.T) {
	th, err := Parse("")
	if err != nil || th != Default() {
		t.Fatalf("空值应返回默认主题: %+v %v", th, err)
	}

	radius := 6
	got, err := Parse(`{"mode":"light","accent":"#ABCDEF","accentDark":"#123456","radius":6,"compact":true}`)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got.Accent != "#abcdef" || got.AccentDark != "#123456" || got.Radius == nil || *got.Radius != radius || !got.Compact {
		t.Fatalf("归一化结果异常: %+v", got)
	}

	raw, err := got.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	round, err := Parse(raw)
	if err != nil || round.Accent != got.Accent || round.Radius == nil || *round.Radius != radius {
		t.Fatalf("序列化回环异常: %+v %v", round, err)
	}

	if _, err := Parse(`{"mode":"neon"}`); err == nil {
		t.Fatal("非法已存配置应报错而不是静默回退")
	}
	if _, err := Parse("not-json"); err == nil {
		t.Fatal("损坏配置应报错")
	}
}
