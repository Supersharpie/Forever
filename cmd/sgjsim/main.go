// sgjsim: batch runner for Sharpie's Gear Judge stat weights on the WoW Forever sim.
// Reads a JSON list of scenarios, runs each one plus one-stat nudges with the same seed,
// and writes DPS / threat per second / damage taken per second for each run.
//
//	go run ./cmd/sgjsim -in scenarios.json -out results.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	gproto "google.golang.org/protobuf/proto"

	"github.com/wowsims/forever/sim/common"
	"github.com/wowsims/forever/sim/core"
	"github.com/wowsims/forever/sim/core/proto"
	"github.com/wowsims/forever/sim/druid/balance"
	"github.com/wowsims/forever/sim/druid/feralbear"
	"github.com/wowsims/forever/sim/druid/feralcat"
	"github.com/wowsims/forever/sim/hunter"
	"github.com/wowsims/forever/sim/mage"
	"github.com/wowsims/forever/sim/warlock"
	paladinprot "github.com/wowsims/forever/sim/paladin/protection"
	"github.com/wowsims/forever/sim/paladin/retribution"
	"github.com/wowsims/forever/sim/priest"
	"github.com/wowsims/forever/sim/rogue"
	"github.com/wowsims/forever/sim/shaman/elemental"
	"github.com/wowsims/forever/sim/shaman/enhancement"
	"github.com/wowsims/forever/sim/warrior/dps"
	"github.com/wowsims/forever/sim/warrior/protection"
)

type Nudge struct {
	Label  string             `json:"label"`
	Stats  map[string]float64 `json:"stats"`  // proto Stat names without the "Stat" prefix, e.g. "Strength"
	Pseudo map[string]float64 `json:"pseudo"` // PseudoStat names without the prefix, e.g. "MeleeHitPercent"
}

type Scenario struct {
	Label       string             `json:"label"`
	Spec        string             `json:"spec"` // "warrior_dps" | "warrior_prot"
	Race        string             `json:"race"` // e.g. "Orc", "Human"
	Talents     string             `json:"talents"`
	MainHand    int32              `json:"mh"`
	OffHand     int32              `json:"oh"`
	AplDir      string             `json:"aplDir"`
	Apl         string             `json:"apl"`
	Stance      string             `json:"stance"` // "Battle" | "Berserker" | "Defensive"
	Buffs       string             `json:"buffs"`  // "raid" | "group" | "none"
	TargetLevel int32              `json:"targetLevel"`
	TargetArmor float64            `json:"targetArmor"` // 0 = the sim's default for the level
	Duration    float64            `json:"duration"`
	Iterations  int32              `json:"iterations"`
	Seed        int64              `json:"seed"`
	Bonus       map[string]float64 `json:"bonus"`
	BonusPseudo map[string]float64 `json:"bonusPseudo"`
	Nudges      []Nudge            `json:"nudges"`
	Tank        bool               `json:"tank"`    // the player tanks the target(s)
	Targets     int                `json:"targets"` // number of targets (default 1)
	TargetDmg   float64            `json:"targetDmg"` // target base swing damage override
	MhImbue     int32              `json:"mhImbue"`   // weapon imbue ids (Rogue poisons: instant 26891, deadly 27186, wound 27188)
	OhImbue     int32              `json:"ohImbue"`
	Class       string             `json:"class"`     // "warrior" (default) | "rogue" | "hunter"
	Ranged      int32              `json:"ranged"`    // ranged slot item id
	Ammo        int32              `json:"ammo"`      // HunterOptions_Ammo value
	PetType     int32              `json:"petType"`   // HunterOptions_PetType value (default Cat)
	PetSpeed    int32              `json:"petSpeed"`  // HunterOptions_PetAttackSpeed value (default 2.0 s)
	Summon      int32              `json:"summon"`    // WarlockOptions_Summon (1 Imp, 2 Voidwalker, 3 Succubus, 4 Felhunter)
	Sacrifice   bool               `json:"sacrifice"` // Demonic Sacrifice the summon
	PactSac     int32              `json:"pactSac"`   // Demonic Pact: the demon sacrificed before the pull
	Front       bool               `json:"front"`     // stand in front of the target (it can parry; solo mobs face you)
}

type Result struct {
	Scenario string  `json:"scenario"`
	Nudge    string  `json:"nudge"`
	Dps      float64 `json:"dps"`
	DpsStdev float64 `json:"dpsStdev"`
	Tps      float64 `json:"tps"`
	Dtps     float64 `json:"dtps"`
	Actions  map[string]float64 `json:"actions,omitempty"`
	PetDps   float64            `json:"petDps,omitempty"`
}

func statIndex(name string, table map[string]int32, prefix string) int32 {
	v, ok := table[prefix+name]
	if !ok {
		log.Fatalf("unknown stat %q", name)
	}
	return v
}

func unitStats(bonus map[string]float64, pseudo map[string]float64) *proto.UnitStats {
	st := make([]float64, len(proto.Stat_name))
	ps := make([]float64, len(proto.PseudoStat_name))
	for k, v := range bonus {
		st[statIndex(k, proto.Stat_value, "Stat")] += v
	}
	for k, v := range pseudo {
		ps[statIndex(k, proto.PseudoStat_value, "PseudoStat")] += v
	}
	return &proto.UnitStats{Stats: st, PseudoStats: ps}
}

func merge(a map[string]float64, b map[string]float64) map[string]float64 {
	m := map[string]float64{}
	for k, v := range a {
		m[k] += v
	}
	for k, v := range b {
		m[k] += v
	}
	return m
}

func stance(s string) proto.WarriorStance {
	switch s {
	case "Battle":
		return proto.WarriorStance_WarriorStanceBattle
	case "Defensive":
		return proto.WarriorStance_WarriorStanceDefensive
	default:
		return proto.WarriorStance_WarriorStanceBerserker
	}
}

func buildRequest(sc Scenario, n Nudge) *proto.RaidSimRequest {
	items := make([]*proto.ItemSpec, proto.ItemSlot_ItemSlotRanged+1)
	for i := range items {
		items[i] = &proto.ItemSpec{}
	}
	items[proto.ItemSlot_ItemSlotMainHand] = &proto.ItemSpec{Id: sc.MainHand}
	items[proto.ItemSlot_ItemSlotOffHand] = &proto.ItemSpec{Id: sc.OffHand}
	if sc.Ranged != 0 {
		items[proto.ItemSlot_ItemSlotRanged] = &proto.ItemSpec{Id: sc.Ranged}
	}

	player := &proto.Player{
		Name:          sc.Label,
		Race:          proto.Race(proto.Race_value["Race"+sc.Race]),
		Class:         proto.Class_ClassWarrior,
		Equipment:     &proto.EquipmentSpec{Items: items},
		TalentsString: sc.Talents,
		BonusStats:    unitStats(merge(sc.Bonus, n.Stats), merge(sc.BonusPseudo, n.Pseudo)),
		Consumables:   &proto.ConsumesSpec{MhImbueId: sc.MhImbue, OhImbueId: sc.OhImbue},
	}
	if sc.Class == "rogue" {
		player.Class = proto.Class_ClassRogue
	}
	if sc.Class == "priest" {
		player.Class = proto.Class_ClassPriest
	}
	if sc.Class == "paladin" {
		player.Class = proto.Class_ClassPaladin
	}
	if sc.Class == "druid" {
		player.Class = proto.Class_ClassDruid
	}
	if sc.Class == "shaman" {
		player.Class = proto.Class_ClassShaman
	}
	if sc.Class == "warlock" {
		player.Class = proto.Class_ClassWarlock
	}
	if sc.Class == "mage" {
		player.Class = proto.Class_ClassMage
	}
	if sc.Class == "hunter" {
		player.Class = proto.Class_ClassHunter
		player.DistanceFromTarget = 25 // shoot from range (under 8 yards a hunter can only melee)
	}
	classOpts := &proto.WarriorOptions{StartingRage: 0, QueueDelay: 250, UseBattleShout: true, DefaultStance: stance(sc.Stance)}
	switch sc.Spec {
	case "priest":
		player.Spec = &proto.Player_DpsPriest{DpsPriest: &proto.DpsPriest{Options: &proto.DpsPriest_Options{ClassOptions: &proto.PriestOptions{
			Armor: proto.PriestOptions_InnerFire, UseShadowfiend: true, PreShadowform: true}}}}
	case "paladin_ret":
		player.Spec = &proto.Player_RetributionPaladin{RetributionPaladin: &proto.RetributionPaladin{Options: &proto.RetributionPaladin_Options{ClassOptions: &proto.PaladinOptions{}}}}
	case "paladin_prot":
		player.Spec = &proto.Player_ProtectionPaladin{ProtectionPaladin: &proto.ProtectionPaladin{Options: &proto.ProtectionPaladin_Options{ClassOptions: &proto.PaladinOptions{}}}}
		player.InFrontOfTarget = true
	case "druid_cat":
		player.Spec = &proto.Player_FeralCatDruid{FeralCatDruid: &proto.FeralCatDruid{
			Rotation: &proto.FeralCatDruid_Rotation{FinishingMove: proto.FeralCatDruid_Rotation_Rip, Biteweave: true, RipMinComboPoints: 5, BiteMinComboPoints: 5, MangleTrick: true},
			Options:  &proto.FeralCatDruid_Options{ClassOptions: &proto.DruidOptions{}}}}
	case "druid_bear":
		player.Spec = &proto.Player_FeralBearDruid{FeralBearDruid: &proto.FeralBearDruid{
			Options:  &proto.FeralBearDruid_Options{ClassOptions: &proto.DruidOptions{}}}}
		player.InFrontOfTarget = true
	case "druid_balance":
		player.Spec = &proto.Player_BalanceDruid{BalanceDruid: &proto.BalanceDruid{Options: &proto.BalanceDruid_Options{ClassOptions: &proto.DruidOptions{}}}}
	case "shaman_enh":
		player.Spec = &proto.Player_EnhancementShaman{EnhancementShaman: &proto.EnhancementShaman{Options: &proto.EnhancementShaman_Options{
			SyncType: proto.ShamanSyncType_Auto, ImbueOh: proto.ShamanImbue(sc.OhImbue),
			ClassOptions: &proto.ShamanOptions{ImbueMh: proto.ShamanImbue(sc.MhImbue)}}}}
		player.Consumables = &proto.ConsumesSpec{}
	case "shaman_ele":
		player.Spec = &proto.Player_ElementalShaman{ElementalShaman: &proto.ElementalShaman{Options: &proto.ElementalShaman_Options{
			ClassOptions: &proto.ShamanOptions{ImbueMh: proto.ShamanImbue(sc.MhImbue)}}}}
		player.Consumables = &proto.ConsumesSpec{}
	case "warlock":
		player.Spec = &proto.Player_Warlock{Warlock: &proto.Warlock{Options: &proto.Warlock_Options{ClassOptions: &proto.WarlockOptions{
			Armor: proto.WarlockOptions_DemonArmor, CurseOptions: proto.WarlockOptions_Elements, Summon: proto.WarlockOptions_Summon(sc.Summon),
			SacrificeSummon: sc.Sacrifice, PactSacrifice: proto.WarlockOptions_Summon(sc.PactSac)}}}}
	case "mage":
		player.Spec = &proto.Player_Mage{Mage: &proto.Mage{Options: &proto.Mage_Options{ClassOptions: &proto.MageOptions{DefaultMageArmor: proto.MageArmor_MageArmorMageArmor}}}}
	case "hunter":
		pet := proto.HunterOptions_PetType(sc.PetType)
		if pet == proto.HunterOptions_PetNone {
			pet = proto.HunterOptions_Cat
		}
		speed := proto.HunterOptions_PetAttackSpeed(sc.PetSpeed)
		if sc.PetSpeed == 0 {
			speed = proto.HunterOptions_Two
		}
		player.Spec = &proto.Player_Hunter{Hunter: &proto.Hunter{Options: &proto.Hunter_Options{ClassOptions: &proto.HunterOptions{
			Ammo: proto.HunterOptions_Ammo(sc.Ammo), PetType: pet, PetAttackSpeed: speed, PetUptime: 1}}}}
	case "rogue":
		player.Spec = &proto.Player_Rogue{Rogue: &proto.Rogue{Options: &proto.Rogue_Options{ClassOptions: &proto.RogueOptions{}}}}
	case "warrior_prot":
		player.Spec = &proto.Player_ProtectionWarrior{ProtectionWarrior: &proto.ProtectionWarrior{Options: &proto.ProtectionWarrior_Options{ClassOptions: classOpts}}}
	default:
		player.Spec = &proto.Player_DpsWarrior{DpsWarrior: &proto.DpsWarrior{Options: &proto.DpsWarrior_Options{ClassOptions: classOpts}}}
	}
	if sc.Front {
		player.InFrontOfTarget = true
	}
	if sc.AplDir != "" {
		player.Rotation = core.GetAplRotation(sc.AplDir, sc.Apl).Rotation
	}

	party := &proto.PartyBuffs{}
	raid := &proto.RaidBuffs{}
	debuffs := &proto.Debuffs{}
	switch sc.Buffs {
	case "raid":
		player.Buffs = &proto.IndividualBuffs{GreaterBlessingOfKings: true, GreaterBlessingOfMight: true}
		raid = &proto.RaidBuffs{GiftOfTheWild: true, PrayerOfFortitude: true, ArcaneBrilliance: true, PrayerOfSpirit: true}
		party = &proto.PartyBuffs{LeaderOfThePack: true, TrueshotAura: true, StrengthOfEarthTotem: true, WindfuryTotem: true, BattleShout: proto.TristateEffect_TristateEffectRegular, MoonkinAura: true, ManaSpringTotem: proto.TristateEffect_TristateEffectRegular}
		debuffs = &proto.Debuffs{SunderArmor: true, FaerieFire: true, CurseOfRecklessness: true, JudgementOfTheCrusader: true, CurseOfElements: true, JudgementOfWisdom: true}
	case "group":
		player.Buffs = &proto.IndividualBuffs{GreaterBlessingOfMight: true}
		raid = &proto.RaidBuffs{GiftOfTheWild: true, PrayerOfFortitude: true, ArcaneBrilliance: true}
		party = &proto.PartyBuffs{BattleShout: proto.TristateEffect_TristateEffectRegular}
		debuffs = &proto.Debuffs{SunderArmor: true}
	}

	target := gproto.Clone(core.NewDefaultTarget()).(*proto.Target)
	if sc.TargetLevel > 0 {
		target.Level = sc.TargetLevel
	}
	if sc.TargetArmor > 0 {
		target.Stats[proto.Stat_StatArmor] = sc.TargetArmor
	}
	if sc.TargetDmg > 0 {
		target.MinBaseDamage = sc.TargetDmg
	}
	rp := core.SinglePlayerRaidProto(player, party, raid, debuffs)
	if sc.Tank {
		rp.Tanks = []*proto.UnitReference{{Type: proto.UnitReference_Player, Index: 0}}
	}
	targets := []*proto.Target{target}
	for i := 1; i < sc.Targets; i++ {
		targets = append(targets, gproto.Clone(target).(*proto.Target))
	}
	return &proto.RaidSimRequest{
		Raid: rp,
		Encounter: &proto.Encounter{Duration: sc.Duration, Targets: targets,
			ExecuteProportion_20: 0.2, ExecuteProportion_25: 0.25, ExecuteProportion_35: 0.35},
		SimOptions: &proto.SimOptions{
			Iterations: sc.Iterations,
			RandomSeed: sc.Seed,
		},
	}
}

func main() {
	in := flag.String("in", "scenarios.json", "scenario list (JSON)")
	out := flag.String("out", "", "results file (JSON); stdout if empty")
	flag.Parse()

	dps.RegisterDpsWarrior()
	rogue.RegisterRogue()
	hunter.RegisterHunter()
	mage.RegisterMage()
	warlock.RegisterWarlock()
	priest.RegisterPriest()
	retribution.RegisterRetributionPaladin()
	paladinprot.RegisterProtectionPaladin()
	feralcat.RegisterFeralCatDruid()
	feralbear.RegisterFeralBearDruid()
	balance.RegisterBalanceDruid()
	elemental.RegisterElementalShaman()
	enhancement.RegisterEnhancementShaman()
	protection.RegisterProtectionWarrior()
	common.RegisterAllEffects()

	data, err := os.ReadFile(*in)
	if err != nil {
		log.Fatal(err)
	}
	var scenarios []Scenario
	if err := json.Unmarshal(data, &scenarios); err != nil {
		log.Fatal(err)
	}

	var results []Result
	for _, sc := range scenarios {
		runs := append([]Nudge{{Label: "base"}}, sc.Nudges...)
		for _, n := range runs {
			res := core.RunRaidSimConcurrent(buildRequest(sc, n))
			if res.Error != nil {
				log.Fatalf("%s / %s: %s", sc.Label, n.Label, res.Error.Message)
			}
			p := res.RaidMetrics.Parties[0].Players[0]
			r := Result{Scenario: sc.Label, Nudge: n.Label, Dps: p.Dps.Avg, DpsStdev: p.Dps.Stdev, Tps: p.Threat.Avg, Dtps: p.Dtps.Avg}
			for _, pet := range p.Pets { // a pet's damage counts toward its owner's
				r.Dps += pet.Dps.Avg
				r.PetDps += pet.Dps.Avg
			}
			if n.Label == "base" {
				r.Actions = map[string]float64{}
				for _, a := range p.Actions {
					key := a.Id.String()
					for _, t := range a.Targets {
						r.Actions[key] += t.Damage / float64(sc.Duration)
					}
				}
			}
			results = append(results, r)
			fmt.Fprintf(os.Stderr, "%-34s %-14s dps %8.1f  tps %8.1f  dtps %7.1f\n", sc.Label, n.Label, r.Dps, r.Tps, r.Dtps)
		}
	}
	enc, _ := json.MarshalIndent(results, "", " ")
	if *out == "" {
		fmt.Println(string(enc))
	} else if err := os.WriteFile(*out, enc, 0o644); err != nil {
		log.Fatal(err)
	}
}
