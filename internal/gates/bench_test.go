package gates

import (
	"runtime"
	"testing"
)

// BenchmarkDetectorBuild builds the detector as production does and runs one detection
// per supported language, so every n-gram model is loaded, and reports the memory the
// process holds from the OS (runtime Sys) in MB once that first build is done.
func BenchmarkDetectorBuild(b *testing.B) {
	samples := []string{
		english,
		"Vous concevrez et maintiendrez des services backend en Go avec notre équipe.",
		"Diseñarás y mantendrás servicios backend en Go junto a nuestro equipo.",
		"Sie entwickeln und pflegen Backend-Dienste in Go gemeinsam mit unserem Team.",
		"Je ontwerpt en onderhoudt backenddiensten in Go samen met ons team.",
		"Vei proiecta și întreține servicii backend în Go alături de echipa noastră.",
		"Progetterai e manterrai servizi backend in Go insieme al nostro team.",
		"Vais desenhar e manter serviços backend em Go com a nossa equipa.",
		"Będziesz projektować i utrzymywać usługi backendowe w Go razem z naszym zespołem.",
	}
	detectAll := func() {
		d := buildDetector()
		for _, s := range samples {
			d.ComputeLanguageConfidenceValues(s)
		}
	}
	// Memory is read once, after the first build has loaded every model: production builds
	// the detector once per process, so repeated builds in the timing loop below would
	// only add allocator churn to the number.
	detectAll()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	mb := float64(m.Sys) / (1 << 20)

	for b.Loop() {
		detectAll()
	}
	b.ReportMetric(mb, "sys-MB")
	b.Logf("detector memory after one build: runtime Sys %.0f MB, heap in use %.0f MB", mb, float64(m.HeapInuse)/(1<<20))
}
