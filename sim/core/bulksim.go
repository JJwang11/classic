package core

import (
	"fmt"
	"math"
	"runtime"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	goproto "google.golang.org/protobuf/proto"

	"github.com/wowsims/classic/sim/core/proto"
	"github.com/wowsims/classic/sim/core/simsignals"
)

const (
	defaultIterationsPerCombo = 1000
)

// raidSimRunner runs a standard raid simulation.
type raidSimRunner func(*proto.RaidSimRequest, chan *proto.ProgressMetrics, bool, simsignals.Signals) *proto.RaidSimResult

// bulkSimRunner runs a bulk simulation.
type bulkSimRunner struct {
	// SingleRaidSimRunner used to run one simulation of the bulk.
	SingleRaidSimRunner raidSimRunner
	// Request used for this bulk simulation.
	Request *proto.BulkSimRequest
}

func BulkSim(signals simsignals.Signals, request *proto.BulkSimRequest, progress chan *proto.ProgressMetrics) *proto.BulkSimResult {
	bulk := &bulkSimRunner{
		SingleRaidSimRunner: runSim,
		Request:             request,
	}

	result := bulk.Run(signals, progress)

	if progress != nil {
		progress <- &proto.ProgressMetrics{
			FinalBulkResult: result,
		}
		close(progress)
	}

	return result
}

type singleBulkSim struct {
	req *proto.RaidSimRequest
	cl  *raidSimRequestChangeLog
	eq  *equipmentSubstitution
}

func (b *bulkSimRunner) Run(signals simsignals.Signals, progress chan *proto.ProgressMetrics) (result *proto.BulkSimResult) {
	defer func() {
		if err := recover(); err != nil {
			result = &proto.BulkSimResult{
				Error: &proto.ErrorOutcome{Message: fmt.Sprintf("%v\nStack Trace:\n%s", err, string(debug.Stack()))},
			}
		}
		signals.Abort.Trigger()
	}()

	// Bulk simming is only supported for the single-player use (i.e. not whole raid-wide simming).
	// Verify that we have exactly 1 player.
	var playerCount int
	var player *proto.Player
	for _, p := range b.Request.GetBaseSettings().GetRaid().GetParties() {
		for _, pl := range p.GetPlayers() {
			// TODO(Riotdog-GehennasEU): Better way to check if a player is valid/set?
			if pl.Name != "" {
				player = pl
				playerCount++
			}
		}
	}
	if playerCount != 1 || player == nil {
		return &proto.BulkSimResult{
			Error: &proto.ErrorOutcome{
				Message: fmt.Sprintf("bulksim: expected exactly 1 player, found %d", playerCount),
			},
		}
	}
	if player.GetDatabase() != nil {
		addToDatabase(player.GetDatabase())
	}
	// reduce to just base party.
	b.Request.BaseSettings.Raid.Parties = []*proto.Party{b.Request.BaseSettings.Raid.Parties[0]}
	// clean to reduce memory
	player.Database = nil

	// Gemming for now can happen before slots are decided.
	// We might have to add logic after slot decisions if we want to enforce keeping meta gem active.

	iterations := b.Request.GetBulkSettings().GetIterationsPerCombo()
	if iterations <= 0 {
		iterations = defaultIterationsPerCombo
	}

	items := b.Request.GetBulkSettings().GetItems()
	// numItems := len(items)
	// if b.Request.BulkSettings.Combinations && numItems > maxItemCount {
	// 	return nil, fmt.Errorf("too many items specified (%d > %d), not computationally feasible", numItems, maxItemCount)
	// }

	// Create all distinct combinations of (item, slot). For example, let's say the only item we
	// want to bulk sim is a one-handed item that can be worn both as an off-hand or a main-hand weapon.
	// For each slot, we will create one itemWithSlot pair, so (item, off-hand) and (item, main-hand).
	// We verify later that we are not emitting any invalid equipment set.
	var distinctItemSlotCombos []*itemWithSlot
	for index, is := range items {
		item, ok := ItemsByID[is.Id]
		if !ok {
			return &proto.BulkSimResult{
				Error: &proto.ErrorOutcome{
					Message: fmt.Sprintf("unknown item with id %d in bulk settings", is.Id),
				},
			}
		}
		for _, slot := range eligibleSlotsForItem(item) {
			distinctItemSlotCombos = append(distinctItemSlotCombos, &itemWithSlot{
				Item:  is,
				Slot:  slot,
				Index: index,
			})
		}
	}
	baseItems := player.Equipment.Items

	allCombos := generateAllEquipmentSubstitutions(signals, baseItems, b.Request.BulkSettings.Combinations, distinctItemSlotCombos)

	var validCombos []singleBulkSim
	count := 0
	for sub := range allCombos {
		count++
		if count > 1000000 {
			panic("over 1 million combos, abandoning attempt")
		}
		substitutedRequest, changeLog := createNewRequestWithSubstitution(b.Request.BaseSettings, sub, b.Request.BulkSettings.AutoEnchant)
		if isValidEquipment(substitutedRequest.Raid.Parties[0].Players[0].Equipment) {
			validCombos = append(validCombos, singleBulkSim{req: substitutedRequest, cl: changeLog, eq: sub})
		}
	}

	// TODO(Riotdog-GehennasEU): Make this configurable?
	maxResults := 30

	var rankedResults []*itemSubstitutionSimResult
	var baseResult *itemSubstitutionSimResult
	newIters := int64(iterations)
	if b.Request.BulkSettings.FastMode {
		newIters /= 100

		// In fast mode try to keep starting iterations between 50 and 1000.
		if newIters < 50 {
			newIters = 50
		}
		if newIters > 1000 {
			newIters = 1000
		}
	}

	maxIterations := newIters * int64(len(validCombos))
	if maxIterations > math.MaxInt32 {
		return &proto.BulkSimResult{
			Error: &proto.ErrorOutcome{Message: fmt.Sprintf("number of total iterations %d too large", maxIterations)},
		}
	}

	for {
		var tempBase *itemSubstitutionSimResult
		var errorOutcome *proto.ErrorOutcome
		// TODO: we could theoretically make getRankedResults accept a channel of validCombos that stream in to it and launches sims as it gets them...
		rankedResults, tempBase, errorOutcome = b.getRankedResults(signals, validCombos, newIters, progress)

		if errorOutcome != nil {
			return &proto.BulkSimResult{Error: errorOutcome}
		}
		// keep replacing the base result with more refined base until we don't have base in the ranked results anymore.
		if tempBase != nil {
			baseResult = tempBase
		}

		// If we aren't doing fast mode, or if halving our results will be less than the maxResults, be done.
		if !b.Request.BulkSettings.FastMode || len(rankedResults) <= maxResults*2 {
			break
		}

		// we have reached max accuracy now
		if newIters >= int64(iterations) {
			break
		}

		// Increase accuracy
		newIters *= 2
		newNumCombos := len(rankedResults) / 2
		validCombos = validCombos[:newNumCombos]
		rankedResults = rankedResults[:newNumCombos]
		for i, comb := range rankedResults {
			validCombos[i] = singleBulkSim{
				req: comb.Request,
				cl:  comb.ChangeLog,
				eq:  comb.Substitution,
			}
		}
	}

	if baseResult == nil {
		return &proto.BulkSimResult{
			Error: &proto.ErrorOutcome{
				Message: "no base result for equipped gear found in bulk sim",
			},
		}
	}

	if len(rankedResults) > maxResults {
		rankedResults = rankedResults[:maxResults]
	}

	bum := baseResult.Result.GetRaidMetrics().GetParties()[0].GetPlayers()[0]
	bum.Actions = nil
	bum.Auras = nil
	bum.Resources = nil
	bum.Pets = nil

	result = &proto.BulkSimResult{
		EquippedGearResult: &proto.BulkComboResult{
			UnitMetrics: bum,
		},
	}

	for _, r := range rankedResults {
		um := r.Result.GetRaidMetrics().GetParties()[0].GetPlayers()[0]
		um.Actions = nil
		um.Auras = nil
		um.Resources = nil
		um.Pets = nil

		result.Results = append(result.Results, &proto.BulkComboResult{
			ItemsAdded:  r.ChangeLog.AddedItems,
			UnitMetrics: um,
		})
	}

	if progress != nil {
		progress <- &proto.ProgressMetrics{
			FinalBulkResult: result,
		}
	}

	return result
}

func (b *bulkSimRunner) getRankedResults(signals simsignals.Signals, validCombos []singleBulkSim, iterations int64, progress chan *proto.ProgressMetrics) ([]*itemSubstitutionSimResult, *itemSubstitutionSimResult, *proto.ErrorOutcome) {
	concurrency := runtime.NumCPU() + 1
	if concurrency <= 0 {
		concurrency = 2
	}

	tickets := make(chan struct{}, concurrency)
	for i := 0; i < concurrency; i++ {
		tickets <- struct{}{}
	}

	results := make(chan *itemSubstitutionSimResult, 10)

	numCombinations := int32(len(validCombos))
	totalIterationsUpperBound := int64(numCombinations) * iterations

	var totalCompletedIterations int32
	var totalCompletedSims int32

	reporterSignal := simsignals.CreateSignals()

	// reporter for all sims combined.
	go func() {
		for !signals.Abort.IsTriggered() && !reporterSignal.Abort.IsTriggered() {
			complIters := atomic.LoadInt32(&totalCompletedIterations)
			complSims := atomic.LoadInt32(&totalCompletedSims)

			// stop reporting
			if complIters == int32(totalIterationsUpperBound) || numCombinations == complSims {
				return
			}

			progress <- &proto.ProgressMetrics{
				TotalSims:           numCombinations,
				CompletedSims:       complSims,
				CompletedIterations: complIters,
				TotalIterations:     int32(totalIterationsUpperBound),
			}
			time.Sleep(time.Second)
		}
	}()

	// launcher for all combos (limited by concurrency max)
	go func() {
		for _, singleCombo := range validCombos {
			<-tickets
			singleSimProgress := make(chan *proto.ProgressMetrics)
			// watches this progress and pushes up to main reporter.
			go func(prog chan *proto.ProgressMetrics) {
				var prevDone int32
				for p := range singleSimProgress {
					delta := p.CompletedIterations - prevDone
					atomic.AddInt32(&totalCompletedIterations, delta)
					prevDone = p.CompletedIterations
					if p.FinalRaidResult != nil {
						break
					}
				}
			}(singleSimProgress)
			// actually run the sim in here.
			go func(sub singleBulkSim) {
				// overwrite the requests iterations with the input for this function.
				sub.req.SimOptions.Iterations = int32(iterations)
				results <- &itemSubstitutionSimResult{
					Request:      sub.req,
					Result:       b.SingleRaidSimRunner(sub.req, singleSimProgress, false, signals),
					Substitution: sub.eq,
					ChangeLog:    sub.cl,
				}
				atomic.AddInt32(&totalCompletedSims, 1)
				tickets <- struct{}{} // when done, allow for new sim to be launched.
			}(singleCombo)
		}
	}()

	rankedResults := make([]*itemSubstitutionSimResult, numCombinations)
	var baseResult *itemSubstitutionSimResult

	for i := range rankedResults {
		result := <-results
		if result.Result == nil || result.Result.Error != nil {
			reporterSignal.Abort.Trigger() // cancel reporter
			return nil, nil, result.Result.Error
		}
		if !result.Substitution.HasItemReplacements() {
			baseResult = result
		}
		rankedResults[i] = result
	}
	reporterSignal.Abort.Trigger() // cancel reporter

	sort.Slice(rankedResults, func(i, j int) bool {
		return rankedResults[i].Score() > rankedResults[j].Score()
	})
	return rankedResults, baseResult, nil
}

// itemSubstitutionSimResult stores the request and response of a simulation, along with the used
// equipment susbstitution and a changelog of which items were added and removed from the base
// equipment set.
type itemSubstitutionSimResult struct {
	Request      *proto.RaidSimRequest
	Result       *proto.RaidSimResult
	Substitution *equipmentSubstitution
	ChangeLog    *raidSimRequestChangeLog
}

// Score used to rank results.
func (r *itemSubstitutionSimResult) Score() float64 {
	if r.Result == nil || r.Result.Error != nil {
		return 0
	}
	return r.Result.RaidMetrics.Dps.Avg
}

// equipmentSubstitution specifies all items to be used as replacements for the equipped gear.
type equipmentSubstitution struct {
	Items []*itemWithSlot
}

// HasChanges returns true if the equipment substitution has any item replacmenets.
func (es *equipmentSubstitution) HasItemReplacements() bool {
	return len(es.Items) > 0
}

func (es *equipmentSubstitution) CanonicalHash() string {
	slotToID := map[proto.ItemSlot]int32{}
	for _, repl := range es.Items {
		slotToID[repl.Slot] = repl.Item.Id
	}

	// Canonical representation always has the ring or trinket with smaller item ID in slot1
	// if the equipment substitution mentions two rings or trinkets.
	if ring1, ok := slotToID[proto.ItemSlot_ItemSlotFinger1]; ok {
		if ring2, ok := slotToID[proto.ItemSlot_ItemSlotFinger2]; ok {
			if ring1 == ring2 {
				return ""
			}
			if ring1 > ring2 {
				slotToID[proto.ItemSlot_ItemSlotFinger1], slotToID[proto.ItemSlot_ItemSlotFinger2] = ring2, ring1
			}
		}
	}
	if trink1, ok := slotToID[proto.ItemSlot_ItemSlotTrinket1]; ok {
		if trink2, ok := slotToID[proto.ItemSlot_ItemSlotTrinket2]; ok {
			if trink1 == trink2 {
				return ""
			}
			if trink1 > trink2 {
				slotToID[proto.ItemSlot_ItemSlotTrinket1], slotToID[proto.ItemSlot_ItemSlotTrinket2] = trink2, trink1
			}
		}
	}

	parts := make([]string, 0, len(proto.ItemSlot_name))
	for i := 0; i < len(proto.ItemSlot_name); i++ {
		if id, ok := slotToID[proto.ItemSlot(i)]; ok {
			parts = append(parts, fmt.Sprintf("%d=%d", i, id))
		}
	}

	return strings.Join(parts, ":")
}

// isValidEquipment returns true if the specified equipment spec is valid. An equipment spec
// is valid if it does not reference a two-hander and off-hand weapon combo.
func isValidEquipment(equipment *proto.EquipmentSpec) bool {
	var usesTwoHander, usesOffhand bool

	// Validate weapons
	if knownItem, ok := ItemsByID[equipment.Items[proto.ItemSlot_ItemSlotMainHand].Id]; ok {
		usesTwoHander = knownItem.HandType == proto.HandType_HandTypeTwoHand
	}
	if knownItem, ok := ItemsByID[equipment.Items[proto.ItemSlot_ItemSlotOffHand].Id]; ok {
		usesOffhand = knownItem.HandType == proto.HandType_HandTypeOffHand
	}
	if usesTwoHander && usesOffhand {
		return false
	}

	// Validate trinkets/rings for duplicate IDs
	if equipment.Items[proto.ItemSlot_ItemSlotFinger1].Id == equipment.Items[proto.ItemSlot_ItemSlotFinger2].Id && equipment.Items[proto.ItemSlot_ItemSlotFinger1].Id != 0 {
		return false
	} else if equipment.Items[proto.ItemSlot_ItemSlotTrinket1].Id == equipment.Items[proto.ItemSlot_ItemSlotTrinket2].Id && equipment.Items[proto.ItemSlot_ItemSlotTrinket1].Id != 0 {
		return false
	}

	// Validate rings/trinkets for heroic/non-heroic (matching name)
	f1, ok1 := ItemsByID[equipment.Items[proto.ItemSlot_ItemSlotFinger1].Id]
	f2, ok2 := ItemsByID[equipment.Items[proto.ItemSlot_ItemSlotFinger2].Id]
	if ok1 && ok2 && f1.Name == f2.Name {
		return false
	}

	t1, ok1 := ItemsByID[equipment.Items[proto.ItemSlot_ItemSlotTrinket1].Id]
	t2, ok2 := ItemsByID[equipment.Items[proto.ItemSlot_ItemSlotTrinket2].Id]
	if ok1 && ok2 && t1.Name == t2.Name {
		return false
	}

	return true
}

// generateAllEquipmentSubstitutions generates all possible valid equipment substitutions for the
// given bulk sim request. Also returns the unchanged equipment ("base equipment set") set as the
// first result. This ensures that simming over all possible equipment substitutions includes the
// base case as well.
func generateAllEquipmentSubstitutions(signals simsignals.Signals, baseItems []*proto.ItemSpec, combinations bool, distinctItemSlotCombos []*itemWithSlot) chan *equipmentSubstitution {
	results := make(chan *equipmentSubstitution)
	go func() {
		defer close(results)

		// No substitutions (base case).
		results <- &equipmentSubstitution{}

		// Organize everything by slot.
		itemsBySlot := make([][]*proto.ItemSpec, 17)
		for _, is := range distinctItemSlotCombos {
			itemsBySlot[is.Slot] = append(itemsBySlot[is.Slot], is.Item)
		}

		if !combinations {
			// seenCombos lets us deduplicate trinket/ring combos.
			comboChecker := ItemComboChecker{}

			// Pre-seed the existing item combos
			comboChecker.HasCombo(baseItems[proto.ItemSlot_ItemSlotFinger1].Id, baseItems[proto.ItemSlot_ItemSlotFinger2].Id)
			comboChecker.HasCombo(baseItems[proto.ItemSlot_ItemSlotTrinket1].Id, baseItems[proto.ItemSlot_ItemSlotTrinket2].Id)

			for slotid, slot := range itemsBySlot {
				for _, item := range slot {
					sub := equipmentSubstitution{
						Items: []*itemWithSlot{{Item: item, Slot: proto.ItemSlot(slotid)}},
					}
					// Handle finger/trinket specially to generate combos
					switch slotid {
					case int(proto.ItemSlot_ItemSlotFinger1), int(proto.ItemSlot_ItemSlotTrinket1):
						if !comboChecker.HasCombo(item.Id, baseItems[slotid+1].Id) {
							results <- &sub
						}
						// Generate extra combos
						subslot := slotid + 1
						for _, subitem := range itemsBySlot[subslot] {
							if shouldSkipCombo(baseItems, subitem, proto.ItemSlot(subslot), comboChecker, sub) {
								continue
							}
							miniCombo := createReplacement(sub, &itemWithSlot{Item: subitem, Slot: proto.ItemSlot(subslot)})
							results <- &miniCombo
						}
					case int(proto.ItemSlot_ItemSlotFinger2), int(proto.ItemSlot_ItemSlotTrinket2):
						// Ensure we don't have this combo with the base equipment.
						if !comboChecker.HasCombo(item.Id, baseItems[slotid-1].Id) {
							results <- &sub
						}
					default:
						results <- &sub
					}
				}
			}
			return
		}

		// Simming all combinations of items. This is useful to find the e.g.
		// the best set of items in your bags.
		subComboChecker := SubstitutionComboChecker{}
		for i := 0; i < len(itemsBySlot); i++ {
			genSlotCombos(proto.ItemSlot(i), baseItems, equipmentSubstitution{}, itemsBySlot, subComboChecker, results)
		}
	}()

	return results
}

func createReplacement(repl equipmentSubstitution, item *itemWithSlot) equipmentSubstitution {
	newItems := make([]*itemWithSlot, len(repl.Items))
	copy(newItems, repl.Items)
	newItems = append(newItems, item)
	repl.Items = newItems
	return repl
}

func shouldSkipCombo(baseItems []*proto.ItemSpec, item *proto.ItemSpec, slot proto.ItemSlot, comboChecker ItemComboChecker, replacements equipmentSubstitution) bool {
	switch slot {
	case proto.ItemSlot_ItemSlotFinger1, proto.ItemSlot_ItemSlotTrinket1:
		return comboChecker.HasCombo(item.Id, baseItems[slot+1].Id)
	case proto.ItemSlot_ItemSlotFinger2, proto.ItemSlot_ItemSlotTrinket2:

		for _, repl := range replacements.Items {
			if slot == proto.ItemSlot_ItemSlotFinger2 && repl.Slot == proto.ItemSlot_ItemSlotFinger1 ||
				slot == proto.ItemSlot_ItemSlotTrinket2 && repl.Slot == proto.ItemSlot_ItemSlotTrinket1 {
				return comboChecker.HasCombo(repl.Item.Id, item.Id)
			}
		}
		// Since we didn't find an item in the opposite slot, check against base items.
		return comboChecker.HasCombo(item.Id, baseItems[slot-1].Id)
	}
	return false
}

func genSlotCombos(slot proto.ItemSlot, baseItems []*proto.ItemSpec, baseRepl equipmentSubstitution, replaceBySlot [][]*proto.ItemSpec, comboChecker SubstitutionComboChecker, results chan *equipmentSubstitution) {
	// Iterate all items in this slot, add to the baseRepl, then descend to add all other item combos.
	for _, item := range replaceBySlot[slot] {
		// Create a new equipment substitution from the current replacements plus the new item.
		combo := createReplacement(baseRepl, &itemWithSlot{Slot: slot, Item: item})
		if comboChecker.HasCombo(combo) {
			continue
		}
		results <- &combo

		// Now descend to each other slot to pair with this combo.
		for j := slot + 1; int(j) < len(replaceBySlot); j++ {
			genSlotCombos(j, baseItems, combo, replaceBySlot, comboChecker, results)
		}
	}
}

// itemWithSlot pairs an item with its fixed item slot.
type itemWithSlot struct {
	Item *proto.ItemSpec
	Slot proto.ItemSlot

	// This index refers to the item's position in the BulkEquipmentSpec of the player and serves as
	// a unique item ID. It is used to verify that a valid equipmentSubstitution only references an
	// item once.
	Index int
}

// raidSimRequestChangeLog stores a change log of which items were added and removed from the base
// equipment set.
type raidSimRequestChangeLog struct {
	AddedItems []*proto.ItemSpecWithSlot
}

// createNewRequestWithSubstitution creates a copy of the input RaidSimRequest and applis the given
// equipment susbstitution to the player's equipment. Copies enchant if specified and possible.
func createNewRequestWithSubstitution(readonlyInputRequest *proto.RaidSimRequest, substitution *equipmentSubstitution, autoEnchant bool) (*proto.RaidSimRequest, *raidSimRequestChangeLog) {
	request := goproto.Clone(readonlyInputRequest).(*proto.RaidSimRequest)
	changeLog := &raidSimRequestChangeLog{}
	player := request.Raid.Parties[0].Players[0]
	equipment := player.Equipment
	for _, is := range substitution.Items {
		oldItem := equipment.Items[is.Slot]
		if autoEnchant && oldItem.Enchant > 0 && is.Item.Enchant == 0 {
			equipment.Items[is.Slot] = goproto.Clone(is.Item).(*proto.ItemSpec)
			equipment.Items[is.Slot].Enchant = oldItem.Enchant
			// TODO: logic to decide if the enchant can be applied to the new item...
			// Specifically, offhand shouldn't get shield enchant
			// Main/One hand shouldn't get staff enchant
			// Later: replace normal enchant if replacement is staff.

			changeLog.AddedItems = append(changeLog.AddedItems, &proto.ItemSpecWithSlot{
				Item: equipment.Items[is.Slot],
				Slot: is.Slot,
			})
		} else {
			equipment.Items[is.Slot] = is.Item
			changeLog.AddedItems = append(changeLog.AddedItems, &proto.ItemSpecWithSlot{
				Item: is.Item,
				Slot: is.Slot,
			})
		}
	}
	return request, changeLog
}

type ItemComboChecker map[int64]struct{}

func (ic *ItemComboChecker) HasCombo(itema int32, itemb int32) bool {
	if itema == itemb {
		return true
	}
	key := ic.generateComboKey(itema, itemb)
	if _, ok := (*ic)[key]; ok {
		return true
	} else {
		(*ic)[key] = struct{}{}
	}
	return false
}

// put this function on ic just so it isn't in global namespace
func (ic *ItemComboChecker) generateComboKey(itemA int32, itemB int32) int64 {
	if itemA > itemB {
		return int64(itemA) + int64(itemB)<<4
	}
	return int64(itemB) + int64(itemA)<<4
}

type SubstitutionComboChecker map[string]struct{}

func (ic *SubstitutionComboChecker) HasCombo(replacements equipmentSubstitution) bool {
	key := replacements.CanonicalHash()
	if key == "" {
		// Invalid combo.
		return true
	}
	if _, ok := (*ic)[key]; ok {
		return true
	}
	(*ic)[key] = struct{}{}
	return false
}
