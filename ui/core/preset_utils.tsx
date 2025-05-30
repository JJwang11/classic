import { IndividualLinkImporter } from './components/individual_sim_ui/importers';
import Toast, { ToastOptions } from './components/toast';
import * as Tooltips from './constants/tooltips.js';
import { Player } from './player.js';
import { APLRotation, APLRotation_Type as APLRotationType } from './proto/apl.js';
import {
	Consumes,
	Debuffs,
	Encounter as EncounterProto,
	EquipmentSpec,
	Faction,
	HealingModel,
	IndividualBuffs,
	Race,
	RaidBuffs,
	Spec,
	UnitReference,
} from './proto/common.js';
import { SavedRotation, SavedTalents } from './proto/ui.js';
import { Stats } from './proto_utils/stats.js';
import { SpecOptions, SpecRotation, specTypeFunctions } from './proto_utils/utils.js';

interface PresetBase {
	name: string;
	tooltip?: string;
	enableWhen?: (obj: Player<any>) => boolean;
	onLoad?: (player: Player<any>) => void;
}

interface PresetOptionsBase extends Pick<PresetBase, 'onLoad'> {
	customCondition?: (player: Player<any>) => boolean;
}

export interface PresetGear extends PresetBase {
	name: string;
	gear: EquipmentSpec;
	tooltip?: string;
	enableWhen?: (obj: Player<any>) => boolean;
}
export interface PresetGearOptions extends PresetOptionsBase, Pick<PresetBase, 'tooltip'> {
	talentTree?: number;
	talentTrees?: Array<number>;
	faction?: Faction;
	customCondition?: (player: Player<any>) => boolean;
}

export interface PresetTalents {
	name: string;
	data: SavedTalents;
	enableWhen?: (obj: Player<any>) => boolean;
}
export interface PresetTalentsOptions {
	customCondition?: (player: Player<any>) => boolean;
}

export interface PresetRotation extends PresetBase {
	name: string;
	rotation: SavedRotation;
	tooltip?: string;
	enableWhen?: (obj: Player<any>) => boolean;
}
export interface PresetRotationOptions extends Pick<PresetOptionsBase, 'onLoad'> {
	talentTree?: number;
	customCondition?: (player: Player<any>) => boolean;
}

export interface PresetEpWeights extends PresetBase {
	epWeights: Stats;
}
export interface PresetEpWeightsOptions extends PresetOptionsBase {}

export interface PresetEncounter extends PresetBase {
	encounter?: EncounterProto;
	healingModel?: HealingModel;
	tanks?: UnitReference[];
	raidBuffs?: RaidBuffs;
	debuffs?: Debuffs;
	buffs?: IndividualBuffs;
	consumes?: Consumes;
}
export interface PresetEncounterOptions extends PresetOptionsBase {}

export interface PresetBuild {
	name: string;
	gear?: PresetGear;
	talents?: PresetTalents;
	rotation?: PresetRotation;
	rotationType?: APLRotationType;
	epWeights?: PresetEpWeights;
	encounter?: PresetEncounter;
	race?: Race;
	options?: Partial<SpecOptions<any>>;
}

export interface PresetBuildOptions extends Omit<PresetBuild, 'name'> {}

export function makePresetGear(name: string, gearJson: any, options?: PresetGearOptions): PresetGear {
	const gear = EquipmentSpec.fromJson(gearJson);
	return makePresetGearHelper(name, gear, options || {});
}

function makePresetGearHelper(name: string, gear: EquipmentSpec, options: PresetGearOptions): PresetGear {
	const conditions: Array<(player: Player<any>) => boolean> = [];
	if (options.talentTree != undefined) {
		conditions.push((player: Player<any>) => player.getTalentTree() == options.talentTree);
	}
	if (options.talentTrees != undefined) {
		conditions.push((player: Player<any>) => (options.talentTrees || []).includes(player.getTalentTree()));
	}
	if (options.faction != undefined) {
		conditions.push((player: Player<any>) => player.getFaction() == options.faction);
	}
	if (options.customCondition != undefined) {
		conditions.push(options.customCondition);
	}

	return {
		name: name,
		tooltip: options.tooltip || Tooltips.BASIC_BIS_DISCLAIMER,
		gear: gear,
		enableWhen: conditions.length > 0 ? (player: Player<any>) => conditions.every(cond => cond(player)) : undefined,
		onLoad: options?.onLoad,
	};
}

export function makePresetTalents(name: string, data: SavedTalents, options?: PresetTalentsOptions): PresetTalents {
	const conditions: Array<(player: Player<any>) => boolean> = [];
	if (options && options.customCondition) {
		conditions.push(options.customCondition);
	}

	return {
		name,
		data,
		enableWhen: conditions.length > 0 ? (player: Player<any>) => conditions.every(cond => cond(player)) : undefined,
	};
}

export const makePresetEpWeights = (name: string, epWeights: Stats, options?: PresetEpWeightsOptions): PresetEpWeights => {
	return makePresetEpWeightHelper(name, epWeights, options || {});
};

const makePresetEpWeightHelper = (name: string, epWeights: Stats, options?: PresetEpWeightsOptions): PresetEpWeights => {
	const conditions: Array<(player: Player<any>) => boolean> = [];
	if (options?.customCondition !== undefined) {
		conditions.push(options.customCondition);
	}

	return {
		name,
		epWeights,
		enableWhen: !!conditions.length ? (player: Player<any>) => conditions.every(cond => cond(player)) : undefined,
		onLoad: options?.onLoad,
	};
};

export function makePresetAPLRotation(name: string, rotationJson: any, options?: PresetRotationOptions): PresetRotation {
	const rotation = SavedRotation.create({
		rotation: APLRotation.fromJson(rotationJson),
	});
	return makePresetRotationHelper(name, rotation, options);
}

export function makePresetSimpleRotation<SpecType extends Spec>(
	name: string,
	spec: SpecType,
	simpleRotation: SpecRotation<SpecType>,
	options?: PresetRotationOptions,
): PresetRotation {
	const rotation = SavedRotation.create({
		rotation: {
			type: APLRotationType.TypeSimple,
			simple: {
				specRotationJson: JSON.stringify(specTypeFunctions[spec].rotationToJson(simpleRotation)),
			},
		},
	});
	return makePresetRotationHelper(name, rotation, options);
}

function makePresetRotationHelper(name: string, rotation: SavedRotation, options?: PresetRotationOptions): PresetRotation {
	const conditions: Array<(player: Player<any>) => boolean> = [];
	if (options?.talentTree != undefined) {
		conditions.push((player: Player<any>) => player.getTalentTree() == options.talentTree);
	}
	if (options?.customCondition != undefined) {
		conditions.push(options.customCondition);
	}

	return {
		name: name,
		rotation: rotation,
		enableWhen: conditions.length > 0 ? (player: Player<any>) => conditions.every(cond => cond(player)) : undefined,
		onLoad: options?.onLoad,
	};
}

export const makePresetEncounter = (name: string, encounter?: PresetEncounter['encounter'] | string, options?: PresetEncounterOptions): PresetEncounter => {
	let healingModel: PresetEncounter['healingModel'] = undefined;
	let tanks: PresetEncounter['tanks'] = undefined;
	let raidBuffs: PresetEncounter['raidBuffs'] = undefined;
	let debuffs: PresetEncounter['debuffs'] = undefined;
	let buffs: PresetEncounter['buffs'] = undefined;
	let consumes: PresetEncounter['consumes'] = undefined;
	if (typeof encounter === 'string') {
		const parsedUrl = IndividualLinkImporter.tryParseUrlLocation(new URL(encounter));
		const settings = parsedUrl?.settings;
		encounter = settings?.encounter;
		healingModel = settings?.player?.healingModel;
		tanks = settings?.tanks;
		raidBuffs = settings?.raidBuffs;
		debuffs = settings?.debuffs;
		buffs = settings?.player?.buffs;
		consumes = settings?.player?.consumes;
	}

	return {
		name,
		encounter,
		tanks,
		healingModel,
		raidBuffs,
		debuffs,
		buffs,
		consumes,
		...options,
	};
};

export const makePresetBuild = (name: string, { gear, talents, rotation, epWeights, encounter, race, options }: PresetBuildOptions): PresetBuild => {
	return { name, gear, talents, rotation, epWeights, encounter, race, options };
};

export type SpecCheckWarning = {
	condition: (player: Player<any>) => boolean;
	message: string;
};

export const makeSpecChangeWarningToast = (checks: SpecCheckWarning[], player: Player<any>, options?: Partial<ToastOptions>) => {
	const messages: string[] = checks.map(({ condition, message }) => condition(player) && message).filter((m): m is string => !!m);
	if (messages.length)
		new Toast({
			variant: 'warning',
			body: (
				<>
					{messages.map(message => (
						<p>{message}</p>
					))}
				</>
			),
			delay: 5000 * messages.length,
			...options,
		});
};
