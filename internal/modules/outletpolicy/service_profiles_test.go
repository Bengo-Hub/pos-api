package outletpolicy

import "testing"

func TestServiceProfilesRegistryIsConsistent(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range ServiceProfiles() {
		if p.Key == "" || p.Label == "" {
			t.Fatalf("profile with empty key or label: %+v", p)
		}
		if seen[p.Key] {
			t.Fatalf("duplicate profile key %q", p.Key)
		}
		seen[p.Key] = true
		switch p.Workflow {
		case WorkflowJob, WorkflowAppointment, WorkflowQueue:
		default:
			t.Fatalf("profile %q has unknown workflow %q", p.Key, p.Workflow)
		}
		stageSeen := map[string]bool{}
		for _, s := range p.Stages {
			if stageSeen[s.Key] {
				t.Fatalf("profile %q repeats stage %q", p.Key, s.Key)
			}
			stageSeen[s.Key] = true
		}
		if len(p.Stages) > 0 && p.Stages[len(p.Stages)-1].Key != "ready" {
			t.Fatalf("profile %q: last stage must be ready, got %q", p.Key, p.Stages[len(p.Stages)-1].Key)
		}
	}
	for _, want := range []string{
		ProfilePrintingBranding, ProfileSalonBarber, ProfileNailParlour, ProfileSpaWellness,
		ProfileAutoGarage, ProfileCarWash, ProfileLaundryDryCleaning, ProfileTailoringFashion,
		ProfileDeviceRepair, ProfileProfessionalGeneral,
	} {
		if !seen[want] {
			t.Errorf("registry is missing profile %q", want)
		}
	}
}

func TestPrintingProfileDefaults(t *testing.T) {
	p, ok := LookupServiceProfile("  Printing_Branding ")
	if !ok {
		t.Fatal("printing profile not found (lookup must trim and lower-case)")
	}
	if p.Workflow != WorkflowJob {
		t.Errorf("printing workflow = %q, want job", p.Workflow)
	}
	if !p.HasStage("proof") || !p.HasStage("ready") || p.HasStage("washing") {
		t.Error("printing stages wrong")
	}
	if !p.DesignFromScratch || !p.AcceptsAttachments {
		t.Error("printing must accept attachments and offer design from scratch")
	}
	if len(p.DefaultStations) == 0 {
		t.Error("printing must seed a production station")
	}
}

func TestAllowsItemUseCase(t *testing.T) {
	printing, _ := LookupServiceProfile(ProfilePrintingBranding)
	salon, _ := LookupServiceProfile(ProfileSalonBarber)
	cases := []struct {
		profile ServiceProfile
		uc      string
		want    bool
	}{
		{printing, "PRINTING_SERVICE", true},
		{printing, "printing_service", true},
		{printing, "PROFESSIONAL_SERVICE", true},
		{printing, "", true},       // untagged legacy item
		{printing, "RETAIL", true}, // inventory's default for an untagged item
		{printing, "SALON_SERVICE", false},
		{printing, "AUTO_SERVICE", false},
		{salon, "SALON_SERVICE", true},
		{salon, "PRINTING_SERVICE", false},
	}
	for _, c := range cases {
		if got := c.profile.AllowsItemUseCase(c.uc); got != c.want {
			t.Errorf("%s.AllowsItemUseCase(%q) = %v, want %v", c.profile.Key, c.uc, got, c.want)
		}
	}
}

func TestServiceProfileFromMetadataAndDeposit(t *testing.T) {
	if _, ok := ServiceProfileFromMetadata(nil); ok {
		t.Error("nil metadata must resolve no profile")
	}
	if _, ok := ServiceProfileFromMetadata(map[string]any{MetaKeyServiceProfile: "nope"}); ok {
		t.Error("unknown key must resolve no profile")
	}
	meta := map[string]any{MetaKeyServiceProfile: ProfilePrintingBranding}
	p, ok := ServiceProfileFromMetadata(meta)
	if !ok || p.Key != ProfilePrintingBranding {
		t.Fatalf("got %+v ok=%v", p, ok)
	}
	if got := JobDepositPercent(meta, p); got != 70 {
		t.Errorf("default deposit = %v, want 70", got)
	}
	meta[MetaKeyJobDepositPercent] = 50.0
	if got := JobDepositPercent(meta, p); got != 50 {
		t.Errorf("override deposit = %v, want 50", got)
	}
	meta[MetaKeyJobDepositPercent] = 150.0
	if got := JobDepositPercent(meta, p); got != 100 {
		t.Errorf("deposit must clamp to 100, got %v", got)
	}
	meta[MetaKeyJobDepositPercent] = -5.0
	if got := JobDepositPercent(meta, p); got != 0 {
		t.Errorf("deposit must clamp to 0, got %v", got)
	}
}
