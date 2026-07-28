package main

import (
	"context"
	"fmt"
	"log"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"

	"github.com/bengobox/pos-service/internal/ent"
	"github.com/bengobox/pos-service/internal/ent/diagnosiscatalog"
	"github.com/bengobox/pos-service/internal/ent/labtest"
)

// seedLabTests gives a pharmacy tenant a starter price list so the Examination stage's test picker
// isn't empty on day one. Idempotent via deterministic IDs; never overwrites a price an admin has
// since edited (create-only — an existing row is left exactly as configured).
func seedLabTests(ctx context.Context, client *ent.Client, tenantID uuid.UUID) error {
	type def struct {
		name, code, category, sample, unit, refRange string
		price                                        float64
	}
	tests := []def{
		{"Full Blood Count", "FBC", "Haematology", "Blood", "", "", 800},
		{"Haemoglobin", "HB", "Haematology", "Blood", "g/dL", "12.0 - 16.0", 300},
		{"Malaria Rapid Test", "MRDT", "Microbiology", "Blood", "", "Negative", 400},
		{"Blood Sugar (Random)", "RBS", "Biochemistry", "Blood", "mmol/L", "3.9 - 7.8", 300},
		{"Blood Sugar (Fasting)", "FBS", "Biochemistry", "Blood", "mmol/L", "3.9 - 5.5", 300},
		{"Urinalysis", "UA", "Microbiology", "Urine", "", "", 500},
		{"Stool Ova & Cysts", "SOC", "Microbiology", "Stool", "", "No ova seen", 500},
		{"Widal Test", "WID", "Serology", "Blood", "", "Negative", 700},
		{"H. Pylori Antigen", "HPYL", "Serology", "Stool", "", "Negative", 900},
		{"Pregnancy Test (hCG)", "HCG", "Serology", "Urine", "", "Negative", 300},
		{"Liver Function Test", "LFT", "Biochemistry", "Blood", "", "", 2500},
		{"Kidney Function Test", "KFT", "Biochemistry", "Blood", "", "", 2500},
		{"Lipid Profile", "LIP", "Biochemistry", "Blood", "", "", 2000},
		{"Erythrocyte Sedimentation Rate", "ESR", "Haematology", "Blood", "mm/hr", "0 - 20", 400},
	}

	created := 0
	for _, t := range tests {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprintf("bengobox:pos:labtest:%s:%s", tenantID, t.code)))
		if exists, _ := client.LabTest.Query().Where(labtest.ID(id)).Exist(ctx); exists {
			continue
		}
		_, err := client.LabTest.Create().
			SetID(id).
			SetTenantID(tenantID).
			SetName(t.name).
			SetCode(t.code).
			SetCategory(t.category).
			SetPrice(decimal.NewFromFloat(t.price)).
			SetSampleType(t.sample).
			SetUnit(t.unit).
			SetReferenceRange(t.refRange).
			Save(ctx)
		if err != nil {
			// A tenant that already added a test with the same NAME hits the unique index — that's
			// their row winning, which is correct; skip rather than fail the whole seed.
			if ent.IsConstraintError(err) {
				continue
			}
			log.Printf("  ⚠️  seed lab test %s: %v", t.name, err)
			continue
		}
		created++
	}
	if created > 0 {
		log.Printf("  ✓ %d lab tests seeded", created)
	}
	return nil
}

// seedGlobalDiagnoses seeds the PLATFORM-GLOBAL curated diagnosis list (tenant_id = uuid.Nil), so
// every tenant's Examination picker starts with the common outpatient conditions. Clinicians can
// still type anything else — that saves a tenant-scoped row alongside these. Global-only, seeded
// once in main() rather than per tenant (mirrors the shared-reference-data rule).
func seedGlobalDiagnoses(ctx context.Context, client *ent.Client) error {
	type def struct{ name, code, category string }
	diagnoses := []def{
		{"Upper Respiratory Tract Infection", "J06.9", "Respiratory"},
		{"Lower Respiratory Tract Infection", "J22", "Respiratory"},
		{"Pneumonia", "J18.9", "Respiratory"},
		{"Asthma", "J45.9", "Respiratory"},
		{"Malaria", "B54", "Infectious"},
		{"Typhoid Fever", "A01.0", "Infectious"},
		{"Urinary Tract Infection", "N39.0", "Genitourinary"},
		{"Gastroenteritis", "K52.9", "Gastrointestinal"},
		{"Peptic Ulcer Disease", "K27.9", "Gastrointestinal"},
		{"Amoebiasis", "A06.9", "Gastrointestinal"},
		{"Hypertension", "I10", "Cardiovascular"},
		{"Type 2 Diabetes Mellitus", "E11", "Endocrine"},
		{"Anaemia", "D64.9", "Haematology"},
		{"Migraine", "G43.9", "Neurological"},
		{"Allergic Rhinitis", "J30.9", "Respiratory"},
		{"Dermatitis", "L30.9", "Dermatology"},
		{"Conjunctivitis", "H10.9", "Ophthalmology"},
		{"Otitis Media", "H66.9", "ENT"},
		{"Tonsillitis", "J03.9", "ENT"},
		{"Musculoskeletal Pain", "M79.1", "Musculoskeletal"},
	}

	created := 0
	for _, d := range diagnoses {
		id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("bengobox:pos:diagnosis:global:"+d.code))
		if exists, _ := client.DiagnosisCatalog.Query().Where(diagnosiscatalog.ID(id)).Exist(ctx); exists {
			continue
		}
		_, err := client.DiagnosisCatalog.Create().
			SetID(id).
			SetTenantID(uuid.Nil).
			SetIsGlobal(true).
			SetName(d.name).
			SetCode(d.code).
			SetCategory(d.category).
			Save(ctx)
		if err != nil {
			if ent.IsConstraintError(err) {
				continue
			}
			log.Printf("  ⚠️  seed diagnosis %s: %v", d.name, err)
			continue
		}
		created++
	}
	if created > 0 {
		log.Printf("  ✓ %d global diagnoses seeded", created)
	}
	return nil
}
