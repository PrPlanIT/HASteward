package triage

import (
	"github.com/PrPlanIT/HASteward/src/output"
	"github.com/PrPlanIT/HASteward/src/output/model"
)

// renderDiagnoses prints triage's diagnosis catalog — each a named failure condition
// paired with the safe remedy that resolves it. Shared by every engine's Analyze so a
// recognized condition is presented identically everywhere (CNPG, Galera, future). A
// multi-line Detail/Remedy is printed verbatim; no-op when the catalog is empty.
func renderDiagnoses(diagnoses []model.Diagnosis) {
	for _, d := range diagnoses {
		output.Println()
		output.Section("DIAGNOSIS: " + d.ID)
		output.Println(d.Summary)
		if d.Detail != "" {
			output.Println(d.Detail)
		}
		if d.Target != "" {
			output.Field("Remediation target", d.Target)
		}
		if d.Remedy != "" {
			output.Field("Remedy", d.Remedy)
		}
	}
}
