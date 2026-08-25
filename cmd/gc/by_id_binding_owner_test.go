package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/storeref/storereftest"
)

// convoyCityConfig loads the city config the convoy resolver takes, so a test
// can call resolveOwningStoreDir the way its production callers do.
func convoyCityConfig(t *testing.T, cityPath string) *config.City {
	t.Helper()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loading the city config at %s: %v", cityPath, err)
	}
	return cfg
}

// resolveThroughTheConvoyScan is the convoy arm's by-id resolution, as its
// callers reach it.
func resolveThroughTheConvoyScan(t *testing.T, cityPath, id string) (beads.Store, string) {
	t.Helper()
	store, dir, err := resolveOwningStoreDir(id, convoyCityConfig(t, cityPath), cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		t.Fatalf("resolving the store that owns %s: %v", id, err)
	}
	return store, dir
}

// TestConvoyResolutionServesTheBindingCopy adds the convoy arm to the
// cross-plane binding-wins property.
//
// This is the arm the property was missing, and it is missing for a structural
// reason rather than an oversight: the convoy resolver's work axis is a scan of
// the city's DIRECTORIES, and a relocated class binding is not one of them. So
// before the binding leg went in front, an infrastructure bead here was not
// merely unrouted — it was answered, successfully, by the copy `gc storage
// migrate` retained in the city store, and the close that followed wrote
// through it.
func TestConvoyResolutionServesTheBindingCopy(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	resident, classStore := classResidentWorkShapedBead(t, cityPath, shadow.ID, "the class-binding copy")
	control, err := work.Create(beads.Bead{Title: "a work bead the binding never held", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the control: %v", err)
	}

	storereftest.RunBindingWins(t,
		storereftest.BindingWinsStores{
			Binding:       classStore,
			Work:          work,
			DualID:        resident.ID,
			BindingTitle:  "the class-binding copy",
			WorkOnlyID:    control.ID,
			WorkOnlyTitle: "a work bead the binding never held",
		},
		storereftest.BindingWinsSurface{
			Name: "the gc convoy by-id resolver",
			Get: func(t *testing.T, id string) beads.Bead {
				t.Helper()
				store, _ := resolveThroughTheConvoyScan(t, cityPath, id)
				b, err := store.Get(id)
				if err != nil {
					t.Fatalf("reading %s from the resolved store: %v", id, err)
				}
				return b
			},
			Close: func(t *testing.T, id string) {
				t.Helper()
				store, _ := resolveThroughTheConvoyScan(t, cityPath, id)
				if err := store.Close(id); err != nil {
					t.Fatalf("closing %s through the resolved store: %v", id, err)
				}
			},
		})
}

// TestConvoyResolutionReportsTheCityDirForABindingHit pins the store-ref
// argument the resolver's doc makes.
//
// The directory this returns is mapped to a store-ref that scopes molecule-root
// lookups. A relocated bead lived in the city store and carried the city's ref
// before the migration moved it, and a binding is not a rig and has no ref of
// its own — so reporting anything but the city path here would strand every
// root recorded before the move.
func TestConvoyResolutionReportsTheCityDirForABindingHit(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	resident, classStore := classResidentWorkShapedBead(t, cityPath, "gc-relic1", "a relocated patrol root")

	store, dir := resolveThroughTheConvoyScan(t, cityPath, resident.ID)
	if store != classStore {
		t.Errorf("the convoy resolver returned %p for %s, want the class binding %p", store, resident.ID, classStore)
	}
	if dir != cityPath {
		t.Errorf("the convoy resolver reported dir %q for a binding hit, want the city path %q — the store-ref these beads carried before the migration is the city's", dir, cityPath)
	}
}

// TestConvoyResolutionDoesNotRefuseDualResidenceAsAmbiguous is the deliberate
// short-circuit, asserted.
//
// The scan REFUSES an id present in more than one candidate store, which is
// right when two ledgers disagree by accident. Dual residency is not that: the
// migration copies with ids preserved and deletes nothing, so a relocated bead
// is supposed to exist twice and has a known winner. A resolver that reached
// the uniqueness rule here would refuse every convoy command on exactly the
// cities that finished migrating.
func TestConvoyResolutionDoesNotRefuseDualResidenceAsAmbiguous(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	resident, classStore := classResidentWorkShapedBead(t, cityPath, shadow.ID, "the class-binding copy")

	store, _, err := resolveOwningStoreDir(resident.ID, convoyCityConfig(t, cityPath), cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	})
	if err != nil {
		t.Fatalf("a dual-resident id resolved to %v; dual residency is the migration working, not two ledgers disagreeing", err)
	}
	if store != classStore {
		t.Errorf("a dual-resident id resolved %p, want the class binding %p", store, classStore)
	}
}

// TestConvoyResolutionUnchangedOnACityThatRelocatesNothing is the compatibility
// row. A city with no [storage] binding plans no binding leg, so the scan runs
// exactly as it did — including its uniqueness refusal, which the binding
// short-circuit must not have disarmed for everyone.
func TestConvoyResolutionUnchangedOnACityThatRelocatesNothing(t *testing.T) {
	cityPath := oneShotCLICity(t, "")
	refuseInfraMigrationSource(t)
	captureCLIStorageStderr(t)
	work := workStoreFor(t, cityPath)
	bead, err := work.Create(beads.Bead{Title: "an ordinary work bead", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}

	store, dir := resolveThroughTheConvoyScan(t, cityPath, bead.ID)
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("reading %s back: %v", bead.ID, err)
	}
	if got.Title != "an ordinary work bead" {
		t.Errorf("the scan served %q, want the work copy", got.Title)
	}
	if dir != cityPath {
		t.Errorf("the scan reported dir %q, want %q", dir, cityPath)
	}

	if _, _, err := resolveOwningStoreDir("gc-nothing-here", convoyCityConfig(t, cityPath), cityPath, func(storeDir string) (beads.Store, error) {
		return openStoreAtForCity(storeDir, cityPath)
	}); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("an absent id resolved to %v, want beads.ErrNotFound — the scan's own miss shape", err)
	}
}

// TestConvoyResolutionSurfacesABindingFaultRatherThanAbsence is the
// classification the whole lane exists for, on this arm. A binding that cannot
// answer must not degrade into a scan of the ledger that holds the stale copy:
// that turns "I could not read the owner" into "the owner is the work store",
// and the write that follows lands where nothing reads.
func TestConvoyResolutionSurfacesABindingFaultRatherThanAbsence(t *testing.T) {
	cityPath := t.TempDir()
	boom := errors.New("binding unreachable")
	seedCLIStorageRoutes(t, cityPath, messagingSplitRoutes(errStore{err: boom}))

	_, _, err := resolveOwningStoreDir("hq-1", nil, cityPath, func(string) (beads.Store, error) {
		return splittest.NewWorkStore(t, "hq"), nil
	})
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("an unreadable binding resolved to err=%v, want the read failure", err)
	}
}

// TestAutocloseOwningStoreAnnouncesAFaultOnce pins the loud-skip.
//
// bd's on_close must not fail because a root could not be resolved, so this
// path swallows every error and falls back to the cwd-rooted store. That is
// right for absence and wrong for a fault, and the difference is invisible
// unless it is said out loud — but only once, because bd closes beads in bursts
// and a repeated line buries the log it wants to be seen in.
func TestAutocloseOwningStoreAnnouncesAFaultOnce(t *testing.T) {
	resetAutocloseFaultOnce(t)
	cityPath, _ := foreignProviderCity(t)
	sink := captureCLIStorageStderr(t)
	failClassBindingReads(t, cityPath, errors.New("the class binding is having a bad day"))

	for range 2 {
		if store, _, ok := autocloseOwningStore("hq-1", cityPath); ok {
			t.Fatalf("a failing binding resolved to %p; the fault must not be answered by the work ledger", store)
		}
	}

	warnings := bytes.Count(sink.Bytes(), []byte("gc autoclose: resolving the store that owns"))
	if warnings != 1 {
		t.Errorf("the fault was announced %d times over two closes, want exactly 1: %s", warnings, sink.String())
	}
	if !bytes.Contains(sink.Bytes(), []byte("bad day")) {
		t.Errorf("the announcement does not carry the store's own cause: %s", sink.String())
	}
}

// TestAutocloseOwningStoreStaysQuietOnAbsence is the control for the test
// above. Absence is the ordinary case — most closed beads are not molecule
// members — and announcing it would make the warning meaningless.
func TestAutocloseOwningStoreStaysQuietOnAbsence(t *testing.T) {
	resetAutocloseFaultOnce(t)
	cityPath, _ := foreignProviderCity(t)
	sink := captureCLIStorageStderr(t)

	if store, _, ok := autocloseOwningStore("hq-nothing-here", cityPath); ok {
		t.Fatalf("an absent id resolved to %p", store)
	}
	if bytes.Contains(sink.Bytes(), []byte("gc autoclose:")) {
		t.Errorf("absence was announced as a fault: %s", sink.String())
	}
}

// TestBeadsShowFallbackServesTheBindingCopy is the read half on the `gc beads
// show` arm: the same scan, taking the first hit rather than refusing a second
// one, and the same retained copy standing in front of the live one.
func TestBeadsShowFallbackServesTheBindingCopy(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	resident, _ := classResidentWorkShapedBead(t, cityPath, shadow.ID, "the class-binding copy")

	var stdout, stderr bytes.Buffer
	if code := doBeadsShowFallback(cityPath, resident.ID, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads show %s exited %d: %s", resident.ID, code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("the class-binding copy")) {
		t.Errorf("gc beads show served %s, want the binding's copy — the work store's is frozen at migration time", stdout.String())
	}
}

// TestBeadsShowFallbackScansForAnIdNoBindingHolds is the control: an id the
// binding never held is still served by the scan, which is what makes this
// about residence rather than about the binding winning everything.
func TestBeadsShowFallbackScansForAnIdNoBindingHolds(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	bead, err := work.Create(beads.Bead{Title: "a work bead the binding never held", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := doBeadsShowFallback(cityPath, bead.ID, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads show %s exited %d: %s", bead.ID, code, stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("a work bead the binding never held")) {
		t.Errorf("gc beads show served %s, want the work copy", stdout.String())
	}
}

// TestBindingOwnerLeavesTheWorkResidualUnprobed is the delegation pin: this
// arm's work axis is a directory scan the resolver must not run, and
// storeref.ResolveBindingOwner is what guarantees it does not.
//
// The measurement is a COUNT rather than a sentinel's refusal, because the
// dangerous case is not a work store that errors — it is one that answers. The
// city store still holds the copy `gc storage migrate` retained, so a work leg
// that got probed here would report a real, stale bead as a binding owner and
// the caller's fall-through would never run. Zero Gets is the only shape that
// rules that out.
func TestBindingOwnerLeavesTheWorkResidualUnprobed(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	counted := &countingGetStore{Store: splittest.NewWorkStore(t, "hq")}
	held, err := counted.Create(beads.Bead{Title: "a bead only the work axis holds", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	counted.gets = 0

	owner, ok, err := byIDBindingOwnerForTopology(cliResidencyTopology(cityPath, nil, counted, nil), held.ID)
	if err != nil {
		t.Fatalf("an id no binding holds resolved to err=%v; a clean miss is ok=false, not a failure", err)
	}
	if ok {
		t.Errorf("the work axis's own copy of %s came back as a binding owner (%p); the caller would never run its scan", held.ID, owner.Store)
	}
	if counted.gets != 0 {
		t.Errorf("the work leg was probed %d time(s), want 0 — the retained pre-migration copy lives there, so a probe answers with a stale bead rather than a miss", counted.gets)
	}

	// The placeholder both call sites hand the plan is defense in depth for the
	// same guarantee: if the executor ever does reach the work leg, it must be a
	// loud error rather than a plausible answer.
	residual := newUnprobedWorkResidual()
	got, err := residual.Get("gc-1")
	if err == nil {
		t.Error("the residual placeholder answered a Get; it must report the contract violation instead of a miss")
	}
	if !strings.Contains(err.Error(), "gc-1") {
		t.Errorf("the residual's Get refusal reads %v, want the probed id named", err)
	}
	_ = got

	// Get is the method the resolver reaches for, so it is the one with a
	// bespoke message — but a leg role that called anything else must not get a
	// nil-pointer panic, and must not get a clean empty answer either.
	if _, err := residual.List(beads.ListQuery{}); !errors.Is(err, errWorkResidualProbed) {
		t.Errorf("the residual answered List with err=%v, want the contract violation — an empty list here reads as absence", err)
	}
	if err := residual.Close("gc-1"); !errors.Is(err, errWorkResidualProbed) {
		t.Errorf("the residual answered Close with err=%v, want the contract violation", err)
	}
	if _, err := residual.Create(beads.Bead{Title: "written through a placeholder"}); !errors.Is(err, errWorkResidualProbed) {
		t.Errorf("the residual answered Create with err=%v, want the contract violation", err)
	}
}

// refusedRelicTopology is a city whose boot could not serve its configured
// split: every infrastructure class resolves to a refusing store and the
// standing refusal rides on the topology. proven says whether a durable census
// has PROVED that binding holds ids outside its reserved namespaces — the one
// fact about a refused binding that is readable without reading the binding,
// because it is a city file rather than a store.
func refusedRelicTopology(proven bool) storeref.Topology {
	refusal := standingStorageRefusal{err: errors.New("storage refused: this city has not converged on its configured [storage] binding; run `gc storage migrate`")}
	classes := infrastructureClasses()
	return assembleResidencyTopology(nil, newUnprobedWorkResidual(), nil, []storeref.ClassBinding{{
		Classes:  classes,
		Prefixes: storeref.ReservedPrefixesFor(classes),
		Leg:      storeref.Leg{Ref: storeref.ClassRef(classes), Store: refusedClassStore{err: refusal}},
		// A refused store declares no mint namespace, so the pessimistic bit
		// stands whatever the memo says.
		HasLegacyResidents:   true,
		KnownLegacyResidents: proven,
	}}, refusal)
}

// TestBindingOwnerRefusedCityWithKnownRelicsRefuses is the ga-q8ick bug.
//
// A refused city still serves WORK, so the resolver tolerates the refusal on a
// residence probe and the surface falls through to its own scan. That is right
// until the binding is PROVEN to hold work-prefixed relics: `gc storage migrate`
// preserved those ids and deleted nothing, so the scan finds the retained
// pre-migration copy in the city work store, serves it, and the close that
// follows writes it. Exit 0, no diagnostic.
func TestBindingOwnerRefusedCityWithKnownRelicsRefuses(t *testing.T) {
	_, ok, err := byIDBindingOwnerForTopology(refusedRelicTopology(true), "gc-1")
	if err == nil {
		t.Fatalf("a refused city proven to hold work-prefixed relics resolved to ok=%v with no error; the caller's scan then serves the copy the migration left in the work store", ok)
	}
	if !storeref.IsStandingRefusal(err) {
		t.Errorf("the refusal came back as %v, want the standing storage refusal that names the remedy", err)
	}
	// The refusal's own sentence is the boot gate's, and a city takes the
	// identical one for an infrastructure-class id it simply cannot serve. What
	// makes this denial actionable is the evidence behind it, which lives in a
	// file the operator has no reason to know exists.
	if !strings.Contains(err.Error(), relicCensusMemoName) {
		t.Errorf("the denial reads %q and never names the note that produced it; the operator sees an ordinary storage refusal and goes looking for a missing bead", err)
	}
	if !strings.Contains(err.Error(), "gc doctor") {
		t.Errorf("the denial reads %q with no route back to a served city", err)
	}
}

// TestBindingOwnerRefusedCityWithoutMemoStillDeclines is the control, and it
// must pass on both sides of the fix.
//
// The memo is TRUE-only: a ref absent from it means "not known", never "known
// clean". So absence must not deny anything — a city that was never migrated,
// or whose note was lost, keeps today's behavior, and work is still served from
// the ledger work never left.
func TestBindingOwnerRefusedCityWithoutMemoStillDeclines(t *testing.T) {
	owner, ok, err := byIDBindingOwnerForTopology(refusedRelicTopology(false), "gc-1")
	if err != nil {
		t.Fatalf("a refused city with no relic evidence resolved to err=%v; the memo's absence is not evidence, and denying here takes work-bead reads away from every unconverged city", err)
	}
	if ok {
		t.Errorf("a refusing binding reported ownership of gc-1 (%p)", owner.Store)
	}
}

// TestResidencyBindingsCarryTheDurableRelicMemo is the wire between the note on
// disk and the plan, and it is the half that cannot be inferred from either end.
//
// The verdict has to survive a boot that cannot read the binding at all, which
// is exactly the boot this matters on: a refused store declares no mint
// namespace, so the live census skips it and learns nothing. The note is a city
// file, so it is readable anyway.
func TestResidencyBindingsCarryTheDurableRelicMemo(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)

	bindings, _ := cliResidencyBindings(cityPath)
	if len(bindings) != 1 {
		t.Fatalf("the fixture resolved %d bindings, want exactly one to write a note about", len(bindings))
	}
	if bindings[0].KnownLegacyResidents {
		t.Fatalf("a city with no relic note reported proven relics; absence of evidence is not evidence")
	}

	if err := writeRelicCensusMemo(cityPath, map[string]bool{string(bindings[0].Leg.Ref): true}); err != nil {
		t.Fatalf("writing the relic-census note: %v", err)
	}
	dropCLIResidencyBindings(filepath.Clean(cityPath))

	noted, _ := cliResidencyBindings(cityPath)
	if len(noted) != 1 {
		t.Fatalf("the re-derived bindings number %d, want one", len(noted))
	}
	if !noted[0].KnownLegacyResidents {
		t.Errorf("the binding %s is named in the relic note and still reads as unproven; the note is not reaching the plan", noted[0].Leg.Ref)
	}
}

// TestRelicNoteProvesNothingAboutAnotherBinding is the note's other half: it is
// keyed by binding REF, and a ref it does not name is "not known".
//
// A per-class split writes refs like "class:g"; a whole split writes
// "class:gmnos". Reading a note that names one as evidence about the other
// would deny by-id reads on a city whose own binding was never censused dirty,
// and the row that pins the positive direction cannot see the difference — with
// one binding in play, "the note names this ref" and "the note is non-empty"
// are the same predicate.
func TestRelicNoteProvesNothingAboutAnotherBinding(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)

	bindings, _ := cliResidencyBindings(cityPath)
	if len(bindings) != 1 {
		t.Fatalf("the fixture resolved %d bindings, want exactly one", len(bindings))
	}
	foreign := string(storeref.ClassRef([]coordclass.Class{coordclass.ClassGraph}))
	if foreign == string(bindings[0].Leg.Ref) {
		t.Fatalf("the foreign ref %q is the fixture's own; the row would prove nothing", foreign)
	}
	if err := writeRelicCensusMemo(cityPath, map[string]bool{foreign: true}); err != nil {
		t.Fatalf("writing the relic-census note: %v", err)
	}
	dropCLIResidencyBindings(filepath.Clean(cityPath))

	noted, _ := cliResidencyBindings(cityPath)
	if len(noted) != 1 {
		t.Fatalf("the re-derived bindings number %d, want one", len(noted))
	}
	if noted[0].KnownLegacyResidents {
		t.Errorf("binding %s reads as proven from a note that names only %s; the note is being read as a flag rather than as a per-ref record", noted[0].Leg.Ref, foreign)
	}
}

// TestUnreadableRelicNoteIsReportedAndFailsOpen pins both halves of the
// corrupt-note contract.
//
// Failing open is deliberate: refusing a topology over an unreadable cache
// turns an optimization into an outage, and "not known" is the direction that
// cannot deny a read. Doing it SILENTLY is not. The note is the only evidence
// that makes a proven-relic city refuse loudly, so a city whose note stopped
// parsing has quietly lost that protection and looks exactly like a city that
// never had one.
func TestUnreadableRelicNoteIsReportedAndFailsOpen(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	// The funnel is already resolved by the fixture, so the boot census has
	// taken its own read of the note and will not take another. What lands on
	// the buffer below is this path's report and nothing else.
	path := relicCensusMemoPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("preparing %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte("{ this is not the note\n"), 0o644); err != nil {
		t.Fatalf("writing a corrupt relic-census note: %v", err)
	}
	dropCLIResidencyBindings(filepath.Clean(cityPath))

	stderr := captureCLIStorageStderr(t)
	bindings, _ := cliResidencyBindings(cityPath)
	if len(bindings) != 1 {
		t.Fatalf("the re-derived bindings number %d, want one", len(bindings))
	}
	if bindings[0].KnownLegacyResidents {
		t.Errorf("an unreadable note proved relics on %s; a note that cannot be read is not evidence", bindings[0].Leg.Ref)
	}
	if !strings.Contains(stderr.String(), path) {
		t.Errorf("the unreadable relic-census note was swallowed; stderr = %q, want it to name %s", stderr.String(), path)
	}
}

// refuseTheseCities installs the same standing storage refusal for several
// cities at once, without standing up a binding for any of them.
//
// seedCLIStorageRoutes cannot be called twice: it resets the whole funnel
// first, so the second call drops the first city's routes. A row that needs a
// city and its control refused at the same moment resets once and seeds both.
func refuseTheseCities(t *testing.T, refusal error, cityPaths ...string) {
	t.Helper()
	resetCLIStorageRoutes(t)
	for _, cityPath := range cityPaths {
		entry := cliStorageRoutesEntryFor(filepath.Clean(cityPath))
		entry.once.Do(func() { entry.routes = refusingStorageRoutes("infra", refusal) })
	}
}

// TestRefusedCityDeniesTheRelicItsOwnCensusRecorded is the ga-q8ick fix as one
// composition, and it is the row that holds the two ends of the note together.
//
// Every other row assembles one end. The census writes the note keyed by the
// ref of the binding IT built; the refused by-id path looks a ref up in that
// note using the ref of the binding the REFUSED funnel built. Those are two
// constructions on two different days of a city's life — one over an opened
// sqlite binding, one over five refusedClassStore values grouped by equality —
// and nothing asserted they spell the ref the same way. They do, because both
// go through storeref.ClassRef over the infrastructure class set, and if either
// side ever narrowed to the classes its own routes happened to name, the lookup
// would miss and the denial would vanish: no error, ok=false, the caller's scan
// serves the pre-migration copy the migration left in the work ledger. Exactly
// the ga-q8ick symptom, restored silently.
//
// So the note here is not written by the test. The city is served, a relic is
// carried into its binding the way `gc storage migrate` does, and the real boot
// census records it. Only then is the city refused.
func TestRefusedCityDeniesTheRelicItsOwnCensusRecorded(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	relic, _ := classResidentWorkShapedBead(t, cityPath, "gc-relic1", "carried across by the migration")

	recorded, err := readRelicCensusMemo(cityPath)
	if err != nil {
		t.Fatalf("reading the note the boot census wrote: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("the boot census recorded %d ref(s) for a city whose binding holds %s; with no note written by the census this row would be asserting against its own fixture", len(recorded), relic.ID)
	}

	// The control city is refused identically and has no note, which is what
	// makes the denial below attributable to the note rather than to the refusal.
	control := t.TempDir()
	refuseTheseCities(t, errors.New("storage refused: this city has not converged on its configured [storage] binding; run `gc storage migrate`"), cityPath, control)

	_, ok, err := cliByIDBindingOwner(cityPath, relic.ID)
	if err == nil {
		t.Fatalf("a refused city whose own census recorded a relic resolved %s to ok=%v with no error; the caller falls through to its scan and serves the copy the migration retained in the work store", relic.ID, ok)
	}
	if !storeref.IsStandingRefusal(err) {
		t.Errorf("the denial came back as %v, want the standing storage refusal", err)
	}
	if !strings.Contains(err.Error(), relicCensusMemoName) {
		t.Errorf("the denial reads %q without naming the note that produced it", err)
	}

	if _, ok, err := cliByIDBindingOwner(control, relic.ID); err != nil || ok {
		t.Errorf("the same refusal over a city with no note resolved to ok=%v err=%v, want a clean fall-through; absence of a note is not evidence, and denying there takes work-bead reads away from every unconverged city", ok, err)
	}
}

// controllerDownForBeadsShow answers the `gc beads show` API seam "no
// controller", which is the only state in which the command reaches the store
// funnel at all.
//
// The seam is stubbed rather than left alone because apiClient() probes for a
// live controller and loads config: unstubbed, the row would pass or fail on
// whether something happened to be listening for the fixture's city, and on a
// machine where one was it would never run the fallback it exists to pin.
func controllerDownForBeadsShow(t *testing.T) {
	t.Helper()
	prev := beadsShowAPIClient
	beadsShowAPIClient = func(string) (*api.Client, string) { return nil, "no controller for this fixture city" }
	t.Cleanup(func() { beadsShowAPIClient = prev })
}

// TestBeadsShowRefusesTheRelicItWouldOtherwiseServeFrozen drives the denial at
// the surface ga-q8ick was reported on.
//
// TestRefusedCityDeniesTheRelicItsOwnCensusRecorded pins the same fixture one
// frame below, at cliByIDBindingOwner, and that is not the same claim. The bead
// exists because `gc beads show` answered with a stale pre-migration row, exit
// 0, no diagnostic — so what has to be pinned is the COMMAND's exit and the
// COMMAND's output, not the error value a helper hands it. Between the two sits
// doBeadsShowFallback's classification, and the failure that reopens ga-q8ick is
// a change there that reads every error as "no binding answered, run your own
// scan": the denial disappears, the scan finds the copy the migration retained
// in the work ledger, and the command reports it as current.
//
// The frozen copy is therefore seeded for real. Without it the row could pass
// against a fall-through that simply found nothing, which is a different exit
// code for a different reason and says nothing about serving stale rows.
func TestBeadsShowRefusesTheRelicItWouldOtherwiseServeFrozen(t *testing.T) {
	const frozenTitle = "the copy the migration left in the work ledger"

	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	frozen, err := work.Create(beads.Bead{Title: frozenTitle, Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store's retained copy: %v", err)
	}
	relic, _ := classResidentWorkShapedBead(t, cityPath, frozen.ID, "the row the binding holds now")

	recorded, err := readRelicCensusMemo(cityPath)
	if err != nil {
		t.Fatalf("reading the note the boot census wrote: %v", err)
	}
	if len(recorded) != 1 {
		t.Fatalf("the boot census recorded %d ref(s) for a city whose binding holds %s; with no note the row below would assert against its own fixture", len(recorded), relic.ID)
	}

	controllerDownForBeadsShow(t)
	refuseTheseCities(t, errors.New("storage refused: this city has not converged on its configured [storage] binding; run `gc storage migrate`"), cityPath)

	var stdout, stderr bytes.Buffer
	code := cmdBeadsShow(relic.ID, "json", &stdout, &stderr)
	if code == 0 {
		t.Fatalf("gc beads show %s exited 0 on a refused city whose own census proved its binding holds relics; stdout = %s", relic.ID, stdout.String())
	}
	if strings.Contains(stdout.String(), frozenTitle) {
		t.Fatalf("gc beads show served the pre-migration copy from the work ledger: %s — this is the ga-q8ick symptom, a row frozen at migration time reported as current", stdout.String())
	}
	if !strings.Contains(stderr.String(), "gc beads show:") {
		t.Errorf("the denial reached stderr as %q without naming the command that refused; an operator reading this cannot tell which surface gave up", stderr.String())
	}
	if !strings.Contains(stderr.String(), relicCensusMemoName) {
		t.Errorf("gc beads show printed %q and never named the note that produced the denial; the operator sees an ordinary storage refusal and goes looking for a missing bead", stderr.String())
	}
	if !strings.Contains(stderr.String(), "gc doctor") {
		t.Errorf("gc beads show printed %q with no route back to a served city", stderr.String())
	}

	// The control, and the reason this row is about the NOTE rather than about
	// the refusal: the same city, the same refusal, the same id, with the
	// evidence removed. A refused city still serves work, so the command must
	// fall through to its scan and answer — from the retained copy, which is the
	// behavior every unconverged city depends on. If this arm ever fails, the pin
	// above has degenerated into "a refused city refuses everything" and would
	// hold against code that dropped the census entirely.
	if err := os.Remove(relicCensusMemoPath(cityPath)); err != nil {
		t.Fatalf("removing the relic-census note for the control: %v", err)
	}
	dropCLIResidencyBindings(filepath.Clean(cityPath))

	var unprovenOut, unprovenErr bytes.Buffer
	if code := cmdBeadsShow(relic.ID, "json", &unprovenOut, &unprovenErr); code != 0 {
		t.Fatalf("gc beads show %s exited %d on the same refusal with no note: %s — absence of evidence is not evidence, and refusing here takes work-bead reads away from every unconverged city", relic.ID, code, unprovenErr.String())
	}
	if !strings.Contains(unprovenOut.String(), frozenTitle) {
		t.Errorf("the unproven arm served %q, want the work ledger's copy; if the scan cannot reach it then the arm above proved nothing about a stale answer being suppressed", unprovenOut.String())
	}
}

// TestBeadsShowServesAConvergedCityThroughTheSameEntryPoint is the healthy
// control for the row above, taken at the same entry point rather than at the
// fallback it calls.
//
// TestBeadsShowFallbackServesTheBindingCopy already pins this property one frame
// down. What it cannot pin is that `gc beads show` still REACHES that frame: a
// guard added to cmdBeadsShow or routeBeadsShow that refused a city whose
// storage looked unusual would leave that row green and break every converged
// city's reads. Exit 0 here, from the binding's copy, is what keeps the denial
// above a statement about proven relics instead of about this funnel.
func TestBeadsShowServesAConvergedCityThroughTheSameEntryPoint(t *testing.T) {
	cityPath, _ := foreignProviderCity(t)
	work := workStoreFor(t, cityPath)
	shadow, err := work.Create(beads.Bead{Title: "the retained work copy", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the work store: %v", err)
	}
	relocated, _ := classResidentWorkShapedBead(t, cityPath, shadow.ID, "the class-binding copy")

	controllerDownForBeadsShow(t)

	var stdout, stderr bytes.Buffer
	if code := cmdBeadsShow(relocated.ID, "json", &stdout, &stderr); code != 0 {
		t.Fatalf("gc beads show %s exited %d on a served city: %s", relocated.ID, code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "the class-binding copy") {
		t.Errorf("gc beads show served %s, want the binding's copy — the work store's is frozen at migration time", stdout.String())
	}
}

// TestConvoyResolutionStillRefusesAnIdTwoLedgersBothHold covers the rule the
// binding short-circuit steps around, which until now nothing asserted anywhere
// in the tree.
//
// The claim "dual residency is not ambiguity" only means something if ambiguity
// is still refused when it is real. Two candidate stores holding the same id
// with no binding in play is two ledgers disagreeing by accident, and the scan
// must say so rather than resolve to whichever candidate it enumerated first —
// the close that followed would write the loser.
func TestConvoyResolutionStillRefusesAnIdTwoLedgersBothHold(t *testing.T) {
	cityPath := t.TempDir()
	seedCLIStorageRoutes(t, cityPath, &storageRoutes{stores: map[coordclass.Class]beads.Store{}})
	cfg := &config.City{Rigs: []config.Rig{{Name: "alpha", Path: filepath.Join(cityPath, "rigs", "alpha")}}}

	byDir := map[string]beads.Store{}
	openStore := func(dir string) (beads.Store, error) {
		if store, ok := byDir[dir]; ok {
			return store, nil
		}
		store := splittest.NewWorkStore(t, "hq")
		if _, err := store.Create(beads.Bead{Title: "a copy in " + dir, Type: "task"}); err != nil {
			t.Fatalf("seeding the candidate at %s: %v", dir, err)
		}
		byDir[dir] = store
		return store, nil
	}

	if len(convoyStoreCandidates(cfg, cityPath, "hq-1")) < 2 {
		t.Fatalf("the fixture offers %d candidate store(s); a uniqueness refusal needs at least two", len(convoyStoreCandidates(cfg, cityPath, "hq-1")))
	}
	_, _, err := resolveOwningStoreDir("hq-1", cfg, cityPath, openStore)
	if err == nil {
		t.Fatal("an id two candidate stores both hold resolved cleanly; the scan's uniqueness rule is gone and the close would write whichever store enumerated last")
	}
	if !strings.Contains(err.Error(), "uniquely addressable") {
		t.Errorf("the refusal reads %v, want the uniqueness contract named", err)
	}
}

// TestBeadForOwnerDoesNotReReadAProbedLeg pins the reason Owner carries a bead
// at all.
//
// A leg the resolver actually probed has already paid for the read, and the
// answer it returns IS that read. Fetching it again is not merely wasteful: it
// opens a window in which the second read disagrees with the one the resolver
// made its ownership decision from.
func TestBeadForOwnerDoesNotReReadAProbedLeg(t *testing.T) {
	counted := &countingGetStore{Store: splittest.NewWorkStore(t, "hq")}
	seeded, err := counted.Create(beads.Bead{Title: "the probed answer", Type: "task"})
	if err != nil {
		t.Fatalf("seeding the counted store: %v", err)
	}

	got, err := beadForOwner(storeref.Owner{Store: counted, Bead: seeded, Read: true}, seeded.ID)
	if err != nil {
		t.Fatalf("reading a probed owner: %v", err)
	}
	if got.Title != "the probed answer" {
		t.Errorf("a probed owner served %q, want the bead the resolver already read", got.Title)
	}
	if counted.gets != 0 {
		t.Errorf("a probed owner cost %d further Get(s), want 0 — the leg's read is the caller's read", counted.gets)
	}

	if _, err := beadForOwner(storeref.Owner{Store: counted}, seeded.ID); err != nil {
		t.Fatalf("reading an unprobed owner: %v", err)
	}
	if counted.gets != 1 {
		t.Errorf("an unprobed owner cost %d Get(s), want exactly 1", counted.gets)
	}
}

// countingGetStore counts the reads a caller makes of its own accord, so a test
// can tell a served answer from a re-fetched one.
type countingGetStore struct {
	beads.Store
	gets int
}

func (s *countingGetStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

// resetAutocloseFaultOnce lets a test observe the once-per-process warning more
// than once per test binary, and leaves the gate closed again afterwards so an
// unrelated test cannot inherit a spent one.
func resetAutocloseFaultOnce(t *testing.T) {
	t.Helper()
	autocloseFaultOnce = sync.Once{}
	t.Cleanup(func() { autocloseFaultOnce = sync.Once{} })
}
