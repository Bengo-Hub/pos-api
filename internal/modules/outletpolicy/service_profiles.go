package outletpolicy

import "strings"

// Service profiles are the sub use cases of a "services" outlet (printing shop, salon, garage,
// laundry, ...). The outlet's use_case stays "services"; the profile lives in
// OutletSetting.metadata["service_profile"] and decides three things:
//
//   - the workflow the terminal runs: a job order that goes through production and is paid at
//     collection, an appointment booked against a staff member, or a walk-in queue;
//   - which inventory SERVICE items the outlet's catalog shows (by inventory item use_case);
//   - the defaults applied when an admin picks the profile (production stations, job stages,
//     the spec fields captured per job line, the default deposit).
//
// This registry is the single source of truth. pos-ui reads it from GET /pos/service-profiles
// instead of keeping its own copy, and the ordering storefront reads the outlet's profile from
// the public outlet endpoint.

// Service workflows.
const (
	WorkflowJob         = "job"
	WorkflowAppointment = "appointment"
	WorkflowQueue       = "queue"
)

// MetaKeyServiceProfile is the OutletSetting.metadata key holding the outlet's profile key.
const MetaKeyServiceProfile = "service_profile"

// MetaKeyJobDepositPercent is the OutletSetting.metadata key holding the default deposit
// (percent of the job total) the cashier is prompted to collect when a job is confirmed.
const MetaKeyJobDepositPercent = "job_deposit_percent"

// Inventory item use cases that mark a SERVICE item as belonging to a service profile.
// PROFESSIONAL_SERVICE is the generic bucket every services profile accepts.
const (
	ItemUseCaseProfessional = "PROFESSIONAL_SERVICE"
	ItemUseCaseSalon        = "SALON_SERVICE"
	ItemUseCaseNail         = "NAIL_SERVICE"
	ItemUseCaseSpa          = "SPA_SERVICE"
	ItemUseCaseAuto         = "AUTO_SERVICE"
	ItemUseCasePrinting     = "PRINTING_SERVICE"
	ItemUseCaseLaundry      = "LAUNDRY_SERVICE"
	ItemUseCaseTailoring    = "TAILORING_SERVICE"
	ItemUseCaseRepair       = "REPAIR_SERVICE"
)

// SpecField describes one per-line detail captured on a job (paper size, vehicle reg, ...).
// Type is one of text | number | select | textarea | date.
type SpecField struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Type        string   `json:"type"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Required    bool     `json:"required,omitempty"`
}

// JobStage is one step of a job's production pipeline, shown on the production board.
type JobStage struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// DefaultStation is a production station created when a job-workflow profile is applied to an
// outlet that has no stations yet. StationType follows the KDS station enum; "all" catches
// every line that no other station claims.
type DefaultStation struct {
	Name           string   `json:"name"`
	StationType    string   `json:"station_type"`
	CategoryFilter []string `json:"category_filter,omitempty"`
}

// ServiceProfile is one services sub use case.
type ServiceProfile struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Workflow    string `json:"workflow"`
	// ItemUseCases are the inventory item use cases whose SERVICE items this profile sells,
	// in addition to PROFESSIONAL_SERVICE.
	ItemUseCases []string `json:"item_use_cases"`
	// SellsRetailGoods lets GOODS in merchandise categories (stationery, apparel blanks,
	// cosmetics, spare parts) through the services catalog gate. A print shop sells paper and
	// branded merchandise; a salon sells hair products.
	SellsRetailGoods bool `json:"sells_retail_goods"`
	// JobLabel is what one unit of work is called in this trade (Job, Repair order, Ticket).
	JobLabel string `json:"job_label"`
	// PerformerLabel names the staff member who does the work (Designer, Stylist, Mechanic).
	PerformerLabel string `json:"performer_label"`
	// ProofLabel names the customer sign-off step (Proof, Estimate, Fitting), empty when the
	// trade has none.
	ProofLabel         string           `json:"proof_label,omitempty"`
	Stages             []JobStage       `json:"stages,omitempty"`
	SpecFields         []SpecField      `json:"spec_fields,omitempty"`
	DefaultStations    []DefaultStation `json:"default_stations,omitempty"`
	DefaultDepositPct  float64          `json:"default_deposit_percent"`
	AcceptsAttachments bool             `json:"accepts_attachments"`
	// DesignFromScratch offers the customer a "design it for me" option instead of attaching
	// their own artwork (printing and branding).
	DesignFromScratch bool `json:"design_from_scratch"`
}

// HasStage reports whether stage is one of the profile's production stages.
func (p ServiceProfile) HasStage(stage string) bool {
	for _, s := range p.Stages {
		if s.Key == stage {
			return true
		}
	}
	return false
}

// AllowsItemUseCase reports whether a SERVICE item tagged with itemUseCase belongs to this
// profile. Untagged items and items still carrying inventory's RETAIL default are allowed so
// an existing tenant's services do not vanish before their catalog is tagged.
func (p ServiceProfile) AllowsItemUseCase(itemUseCase string) bool {
	uc := strings.ToUpper(strings.TrimSpace(itemUseCase))
	if uc == "" || uc == "RETAIL" || uc == ItemUseCaseProfessional {
		return true
	}
	for _, allowed := range p.ItemUseCases {
		if uc == allowed {
			return true
		}
	}
	return false
}

// ServiceItemUseCases lists every inventory item use case that marks a services item. The
// catalog uses it to recognise a SERVICE item that belongs to a different trade.
var ServiceItemUseCases = []string{
	ItemUseCaseProfessional, ItemUseCaseSalon, ItemUseCaseNail, ItemUseCaseSpa, ItemUseCaseAuto,
	ItemUseCasePrinting, ItemUseCaseLaundry, ItemUseCaseTailoring, ItemUseCaseRepair,
}

// Profile keys.
const (
	ProfilePrintingBranding    = "printing_branding"
	ProfileSalonBarber         = "salon_barber"
	ProfileNailParlour         = "nail_parlour"
	ProfileSpaWellness         = "spa_wellness"
	ProfileAutoGarage          = "auto_garage"
	ProfileCarWash             = "car_wash"
	ProfileLaundryDryCleaning  = "laundry_drycleaning"
	ProfileTailoringFashion    = "tailoring_fashion"
	ProfileDeviceRepair        = "device_repair"
	ProfileProfessionalGeneral = "professional_general"
)

var printingProfile = ServiceProfile{
	Key:              ProfilePrintingBranding,
	Label:            "Printing & Branding",
	Description:      "Print shops, large format, merchandise branding and design studios. Jobs are quoted at reception, confirmed with a deposit, produced, then collected and paid in full.",
	Workflow:         WorkflowJob,
	ItemUseCases:     []string{ItemUseCasePrinting},
	SellsRetailGoods: true,
	JobLabel:         "Job",
	PerformerLabel:   "Designer",
	ProofLabel:       "Proof",
	Stages: []JobStage{
		{Key: "design", Label: "Design"},
		{Key: "proof", Label: "Proof approval"},
		{Key: "print", Label: "Printing"},
		{Key: "finishing", Label: "Finishing"},
		{Key: "ready", Label: "Ready for collection"},
	},
	SpecFields: []SpecField{
		{Key: "size", Label: "Size", Type: "text", Placeholder: "A4, A3, 3ft x 6ft, 85 x 55 mm"},
		{Key: "material", Label: "Material", Type: "select", Options: []string{
			"Art paper", "Matte paper", "Glossy paper", "Bond paper", "Card (300gsm)", "Sticker vinyl",
			"Banner (PVC flex)", "Canvas", "Reflective vinyl", "One-way vision", "Fabric / T-shirt",
			"Mug / ceramic", "Other",
		}},
		{Key: "sides", Label: "Sides", Type: "select", Options: []string{"Single sided", "Double sided"}},
		{Key: "colour", Label: "Colour", Type: "select", Options: []string{"Full colour", "Black & white", "Spot colour"}},
		{Key: "finishing", Label: "Finishing", Type: "select", Options: []string{
			"None", "Lamination (matte)", "Lamination (gloss)", "Cutting / trimming", "Binding", "Eyelets",
			"Mounting", "Die-cut", "Folding",
		}},
		{Key: "print_notes", Label: "Print notes", Type: "textarea", Placeholder: "Text to print, colours, placement"},
	},
	DefaultStations: []DefaultStation{
		{Name: "Production", StationType: "all"},
	},
	DefaultDepositPct:  70,
	AcceptsAttachments: true,
	DesignFromScratch:  true,
}

// serviceProfiles is the ordered registry. Order drives the admin picker.
var serviceProfiles = []ServiceProfile{
	printingProfile,
	{
		Key: ProfileSalonBarber, Label: "Salon & Barbershop",
		Description: "Hair salons and barbershops. Book a stylist or barber, pick a style or bring a reference photo.",
		Workflow:    WorkflowAppointment, ItemUseCases: []string{ItemUseCaseSalon},
		SellsRetailGoods: true, JobLabel: "Appointment", PerformerLabel: "Stylist",
		DefaultDepositPct: 0, AcceptsAttachments: true,
	},
	{
		Key: ProfileNailParlour, Label: "Nail Parlour",
		Description: "Manicure, pedicure and nail art. Pick a catalogue design or share your own.",
		Workflow:    WorkflowAppointment, ItemUseCases: []string{ItemUseCaseNail, ItemUseCaseSalon},
		SellsRetailGoods: true, JobLabel: "Appointment", PerformerLabel: "Nail technician",
		DefaultDepositPct: 0, AcceptsAttachments: true,
	},
	{
		Key: ProfileSpaWellness, Label: "Spa & Wellness",
		Description: "Massage, facials, therapies and wellness sessions booked against a therapist and room.",
		Workflow:    WorkflowAppointment, ItemUseCases: []string{ItemUseCaseSpa, ItemUseCaseSalon},
		SellsRetailGoods: true, JobLabel: "Session", PerformerLabel: "Therapist",
		DefaultDepositPct: 0, AcceptsAttachments: false,
	},
	{
		Key: ProfileAutoGarage, Label: "Garage & Auto Service",
		Description: "Vehicle servicing and repairs. Inspect, estimate, get approval, repair, then collect and pay.",
		Workflow:    WorkflowJob, ItemUseCases: []string{ItemUseCaseAuto},
		SellsRetailGoods: true, JobLabel: "Job card", PerformerLabel: "Mechanic", ProofLabel: "Estimate",
		Stages: []JobStage{
			{Key: "inspection", Label: "Inspection"},
			{Key: "estimate", Label: "Estimate approval"},
			{Key: "repair", Label: "In the workshop"},
			{Key: "qc", Label: "Quality check"},
			{Key: "ready", Label: "Ready for collection"},
		},
		SpecFields: []SpecField{
			{Key: "vehicle_reg", Label: "Vehicle reg", Type: "text", Placeholder: "KDA 123A", Required: true},
			{Key: "make_model", Label: "Make & model", Type: "text", Placeholder: "Toyota Axio 2014"},
			{Key: "mileage", Label: "Mileage (km)", Type: "number"},
			{Key: "complaint", Label: "Complaint / warning lights", Type: "textarea"},
		},
		DefaultStations:   []DefaultStation{{Name: "Workshop", StationType: "all"}},
		DefaultDepositPct: 0, AcceptsAttachments: true,
	},
	{
		Key: ProfileCarWash, Label: "Car Wash & Detailing",
		Description: "Walk-in vehicle washing and detailing served from a queue by bay.",
		Workflow:    WorkflowQueue, ItemUseCases: []string{ItemUseCaseAuto},
		SellsRetailGoods: true, JobLabel: "Wash", PerformerLabel: "Washer",
		SpecFields: []SpecField{
			{Key: "vehicle_reg", Label: "Vehicle reg", Type: "text", Required: true},
		},
	},
	{
		Key: ProfileLaundryDryCleaning, Label: "Laundry & Dry Cleaning",
		Description: "Garments are tagged at intake, cleaned in stages and collected against the ticket.",
		Workflow:    WorkflowJob, ItemUseCases: []string{ItemUseCaseLaundry},
		JobLabel: "Ticket", PerformerLabel: "Attendant",
		Stages: []JobStage{
			{Key: "washing", Label: "Washing"},
			{Key: "pressing", Label: "Drying & pressing"},
			{Key: "folding", Label: "Folding & packing"},
			{Key: "ready", Label: "Ready for collection"},
		},
		SpecFields: []SpecField{
			{Key: "tag_number", Label: "Tag number", Type: "text"},
			{Key: "garment_notes", Label: "Stains / care notes", Type: "textarea"},
			{Key: "express", Label: "Service speed", Type: "select", Options: []string{"Standard", "Express"}},
		},
		DefaultStations:   []DefaultStation{{Name: "Laundry", StationType: "all"}},
		DefaultDepositPct: 0,
	},
	{
		Key: ProfileTailoringFashion, Label: "Tailoring & Fashion Design",
		Description: "Made-to-measure and alterations with measurements, fittings and a deposit.",
		Workflow:    WorkflowJob, ItemUseCases: []string{ItemUseCaseTailoring},
		SellsRetailGoods: true, JobLabel: "Order", PerformerLabel: "Tailor", ProofLabel: "Fitting",
		Stages: []JobStage{
			{Key: "cutting", Label: "Cutting"},
			{Key: "sewing", Label: "Sewing"},
			{Key: "fitting", Label: "Fitting"},
			{Key: "finishing", Label: "Finishing"},
			{Key: "ready", Label: "Ready for collection"},
		},
		SpecFields: []SpecField{
			{Key: "measurements", Label: "Measurements", Type: "textarea", Placeholder: "Chest, waist, length, sleeve"},
			{Key: "fabric", Label: "Fabric", Type: "text"},
			{Key: "fitting_date", Label: "Fitting date", Type: "date"},
		},
		DefaultStations:   []DefaultStation{{Name: "Workroom", StationType: "all"}},
		DefaultDepositPct: 50, AcceptsAttachments: true,
	},
	{
		Key: ProfileDeviceRepair, Label: "Phone & Electronics Repair",
		Description: "Device repairs tracked on repair tickets with parts, diagnosis and warranty.",
		Workflow:    WorkflowJob, ItemUseCases: []string{ItemUseCaseRepair},
		SellsRetailGoods: true, JobLabel: "Repair ticket", PerformerLabel: "Technician",
	},
	{
		Key: ProfileProfessionalGeneral, Label: "General Professional Services",
		Description: "Any other service business. Jobs with a brief and attachments, paid at collection.",
		Workflow:    WorkflowJob, ItemUseCases: nil,
		SellsRetailGoods: true, JobLabel: "Job", PerformerLabel: "Staff",
		Stages: []JobStage{
			{Key: "in_progress", Label: "In progress"},
			{Key: "ready", Label: "Ready"},
		},
		SpecFields: []SpecField{
			{Key: "details", Label: "Details", Type: "textarea"},
		},
		DefaultStations:   []DefaultStation{{Name: "Work queue", StationType: "all"}},
		DefaultDepositPct: 0, AcceptsAttachments: true,
	},
}

// ServiceProfiles returns the registry in display order.
func ServiceProfiles() []ServiceProfile {
	out := make([]ServiceProfile, len(serviceProfiles))
	copy(out, serviceProfiles)
	return out
}

// LookupServiceProfile returns the profile for key, or false when key is unknown.
func LookupServiceProfile(key string) (ServiceProfile, bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	for _, p := range serviceProfiles {
		if p.Key == k {
			return p, true
		}
	}
	return ServiceProfile{}, false
}

// ServiceProfileFromMetadata resolves the profile stored on an outlet's settings metadata.
// Returns false when none is set or the stored key is unknown.
func ServiceProfileFromMetadata(meta map[string]any) (ServiceProfile, bool) {
	if meta == nil {
		return ServiceProfile{}, false
	}
	key, _ := meta[MetaKeyServiceProfile].(string)
	if key == "" {
		return ServiceProfile{}, false
	}
	return LookupServiceProfile(key)
}

// JobDepositPercent returns the outlet's configured default deposit, falling back to the
// profile default. Values are clamped to 0..100.
func JobDepositPercent(meta map[string]any, profile ServiceProfile) float64 {
	pct := profile.DefaultDepositPct
	if meta != nil {
		switch v := meta[MetaKeyJobDepositPercent].(type) {
		case float64:
			pct = v
		case int:
			pct = float64(v)
		}
	}
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
