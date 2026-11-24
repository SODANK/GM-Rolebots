package rolebots

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
)

// ensureTables creates every module-owned table (rolebot_ prefix). Idempotent.
func ensureTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		// Roster: which existing characters RoleBot may animate. A row means
		// "assigned"; enabled gates whether the AI may possess it.
		`CREATE TABLE IF NOT EXISTS rolebot_characters (
			characterID BIGINT NOT NULL,
			enabled     TINYINT NOT NULL DEFAULT 0,
			addedDate   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID)
		)`,
		// Characters the module made, versus ones the operator adopted: a marker, not a second system. (seq, slot)
		// is the unique handle: seq is the RoleBotN account number, slot the pilot's place on it. poseSource names a pose-bank entry (poses.go), never a character name.
		`CREATE TABLE IF NOT EXISTS rolebot_generated (
			characterID BIGINT NOT NULL,
			accountID   BIGINT NOT NULL,
			accountName VARCHAR(43) NOT NULL DEFAULT '',
			seq         INT NOT NULL,
			slot        INT NOT NULL DEFAULT 0,
			archetype   VARCHAR(24) NOT NULL DEFAULT '',
			band        VARCHAR(12) NOT NULL DEFAULT '',
			maturity    VARCHAR(12) NOT NULL DEFAULT '',
			bloodlineID BIGINT NOT NULL DEFAULT 0,
			gender      TINYINT NOT NULL DEFAULT 0,
			poseSource  VARCHAR(64) NOT NULL DEFAULT '',
			batchID     BIGINT NOT NULL DEFAULT 0,
			createdDate BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID),
			UNIQUE KEY uq_rbgen_slot (seq, slot),
			KEY idx_rbgen_seq (seq),
			KEY idx_rbgen_acct (accountID),
			KEY idx_rbgen_batch (batchID)
		)`,
		// Global flags shaping ALL activities of one rolebot.
		// List columns are comma-separated IDs ("" = none).
		`CREATE TABLE IF NOT EXISTS rolebot_profiles (
			characterID       BIGINT NOT NULL,
			factionsSupported TEXT NULL,
			factionsHated     TEXT NULL,
			PRIMARY KEY (characterID)
		)`,
		// What the roster last saw in a system's belts (m3 now against the seed): a recording, not a ledger. The ore lives in
		// asteroidFields + asteroidDepletion (beltfield.go); the belt search reads this instead of asking per system. No row means untouched.
		`CREATE TABLE IF NOT EXISTS rolebot_belt_field (
			solarSystemID BIGINT NOT NULL,
			seed          BIGINT NOT NULL DEFAULT 0,
			m3Total       DOUBLE NOT NULL DEFAULT 0,
			m3Left        DOUBLE NOT NULL DEFAULT 0,
			checkedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (solarSystemID),
			KEY idx_rbfield_checked (checkedAt)
		)`,
		// Personality: one row per (character, trait), -100..+100, where 0 or a missing row is the historical behaviour (tuning.go).
		// Key-value, so a new trait needs no DDL, migration or form change.
		`CREATE TABLE IF NOT EXISTS rolebot_traits (
			characterID BIGINT NOT NULL,
			trait       VARCHAR(16) NOT NULL,
			value       INT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, trait)
		)`,
		// Reusable profile templates: profile fields, traits and activity configs as one JSON blob that can be applied to any number of bots.
		`CREATE TABLE IF NOT EXISTS rolebot_profile_templates (
			templateID  BIGINT NOT NULL AUTO_INCREMENT,
			name        VARCHAR(100) NOT NULL DEFAULT '',
			json        MEDIUMTEXT NULL,
			createdDate BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (templateID)
		)`,
		// rolebot_fits is retired (fitgen.go generates fits per pilot) and not dropped, since this module's tables go inert; drop it by hand.
		// rolebot_fit_ships: which hull a character holds for a fit. Ownership, not permission: a row appears when procurement assembles the ship and goes with it (pod.go).
		`CREATE TABLE IF NOT EXISTS rolebot_fit_ships (
			characterID BIGINT NOT NULL,
			fitID       BIGINT NOT NULL,
			shipItemID  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, fitID),
			KEY idx_rbship_char (characterID)
		)`,
		// Rolebot action log, surfaced at /rolebots/log.
		`CREATE TABLE IF NOT EXISTS rolebot_log (
			logID       BIGINT NOT NULL AUTO_INCREMENT,
			ts          BIGINT NOT NULL,
			characterID BIGINT NOT NULL DEFAULT 0,
			level       VARCHAR(8) NOT NULL DEFAULT 'info',
			category    VARCHAR(24) NOT NULL DEFAULT '',
			message     TEXT NULL,
			PRIMARY KEY (logID),
			KEY idx_rblog_ts (ts),
			KEY idx_rblog_char (characterID),
			KEY idx_rblog_level (level, logID),
			KEY idx_rblog_cat_ts (category, ts)
		)`,
		// Persistent sim state, abstract tier: one row per rolebot. phase/data describe the current plan leg, so the sim resumes where it stopped.
		`CREATE TABLE IF NOT EXISTS rolebot_state (
			characterID BIGINT NOT NULL,
			activity    VARCHAR(16) NOT NULL DEFAULT '',
			phase       VARCHAR(32) NOT NULL DEFAULT 'idle',
			data        TEXT NULL,
			updatedAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID)
		)`,
		// A standing group of pilots on one activity: activity + config are set once and obeyed by every member; state is the gang blackboard.
		// autoFormed marks a gang gangform.go built (the only kind the sweep tears down); leaderCharID sized it; expiresAt winds an auto op down.
		`CREATE TABLE IF NOT EXISTS rolebot_gangs (
			gangID       BIGINT NOT NULL AUTO_INCREMENT,
			name         VARCHAR(100) NOT NULL DEFAULT '',
			enabled      TINYINT NOT NULL DEFAULT 1,
			activity     VARCHAR(16) NOT NULL DEFAULT '',
			config       TEXT NULL,
			state        TEXT NULL,
			leaderCharID BIGINT NOT NULL DEFAULT 0,
			corpID       BIGINT NOT NULL DEFAULT 0,
			autoFormed   TINYINT NOT NULL DEFAULT 0,
			expiresAt    BIGINT NOT NULL DEFAULT 0,
			updatedAt    BIGINT NOT NULL DEFAULT 0,
			createdDate  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (gangID)
		)`,
		// recruitCorpID is the corporation a seconded escort (gangescort.go) came from, 0 = the gang's own.
		// It is stored because the live corporationID would hide a changed employer.
		`CREATE TABLE IF NOT EXISTS rolebot_gang_members (
			gangID        BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			role          VARCHAR(16) NOT NULL DEFAULT 'miner',
			recruitCorpID BIGINT NOT NULL DEFAULT 0,
			addedDate     BIGINT NOT NULL DEFAULT 0,
			rider         TINYINT NOT NULL DEFAULT 0,
			PRIMARY KEY (gangID, characterID),
			KEY idx_rbgm_char (characterID)
		)`,
		// Module-made jetcans (cans.go). The entity row is the truth; this registry lets the hauler and the expiry sweep find them.
		// Rows go stale when the core removes a can; sweepCans prunes them.
		`CREATE TABLE IF NOT EXISTS rolebot_gang_cans (
			canItemID   BIGINT NOT NULL,
			gangID      BIGINT NOT NULL,
			ownerCharID BIGINT NOT NULL,
			systemID    BIGINT NOT NULL,
			beltItemID  BIGINT NOT NULL DEFAULT 0,
			createdAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (canItemID),
			KEY idx_rbcan_gang (gangID, systemID)
		)`,
		// Unpaid ore balances of the industry fleet (gangindustry.go), one row per (gang, miner, type): payFleetOre deletes what it pays.
		// haulerCharID is the load a claim rides in: a claim belongs to one hold, and only that hold's drop may settle or delete it.
		`CREATE TABLE IF NOT EXISTS rolebot_gang_ore (
			gangID       BIGINT NOT NULL,
			haulerCharID BIGINT NOT NULL DEFAULT 0,
			characterID  BIGINT NOT NULL,
			typeID       BIGINT NOT NULL,
			units        BIGINT NOT NULL DEFAULT 0,
			updatedAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (gangID, haulerCharID, characterID, typeID)
		)`,
		// The op's work log (gangreport.go), per pilot and type, never subtracted from; gang_ore and gang_loot are books of what is owed and empty out.
		// kind is ore | loot | kill (a kill row's typeID is the rat, units its body count); isk is the pay or bounty.
		`CREATE TABLE IF NOT EXISTS rolebot_gang_haul (
			gangID      BIGINT NOT NULL,
			characterID BIGINT NOT NULL,
			kind        VARCHAR(8) NOT NULL DEFAULT '',
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			units       BIGINT NOT NULL DEFAULT 0,
			m3          DOUBLE NOT NULL DEFAULT 0,
			isk         DOUBLE NOT NULL DEFAULT 0,
			updatedAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (gangID, characterID, kind, typeID),
			KEY idx_rbhaul_gang (gangID, kind)
		)`,
		// Ore a mining fleet delivered into its commander's hangar, not yet disposed of (gangindustry.go): marks which hangar ore is the fleet's.
		// Character-keyed, not gang-keyed: rows must outlive a fleet hard-killed by sweepAutoGangs, and the commander's next home loop clears them.
		`CREATE TABLE IF NOT EXISTS rolebot_ore_hold (
			characterID BIGINT NOT NULL,
			stationID   BIGINT NOT NULL,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			units       BIGINT NOT NULL DEFAULT 0,
			takenAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, stationID, typeID),
			KEY idx_rboreh_char (characterID)
		)`,
		// Pilots serving in a player's fleet (conscript.go). gangID is a core gang.Registry ID, renumbered each boot, so rows are boot-scoped (releaseAllConscripts).
		// Not a rolebot_gangs row: a gangID there would exclude the pilot from the one-second grid lane (gridlane.go).
		`CREATE TABLE IF NOT EXISTS rolebot_conscripts (
			characterID   BIGINT NOT NULL,
			gangID        BIGINT NOT NULL DEFAULT 0,
			bossCharID    BIGINT NOT NULL DEFAULT 0,
			bossCorpID    BIGINT NOT NULL DEFAULT 0,
			role          VARCHAR(12) NOT NULL DEFAULT '',
			dutyStationID BIGINT NOT NULL DEFAULT 0,
			joinedAt      BIGINT NOT NULL DEFAULT 0,
			lastOrderAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID),
			KEY idx_rbcon_gang (gangID),
			KEY idx_rbcon_boss (bossCharID)
		)`,
		// Which players have been sent the command reference, keyed by the player (the briefing is about what its reader knows).
		// sends counts re-requests, a sign the command list is hard to find.
		`CREATE TABLE IF NOT EXISTS rolebot_conscript_briefed (
			charID BIGINT NOT NULL,
			sentAt BIGINT NOT NULL DEFAULT 0,
			sends  INT NOT NULL DEFAULT 0,
			PRIMARY KEY (charID)
		)`,
		// Which fleet manual (fcmanual.go) each player has been mailed, by kind (conscript, director, rider) and version.
		// A version below fcManualVersion is mailed the new manual once.
		`CREATE TABLE IF NOT EXISTS rolebot_fc_manual_sent (
			charID  BIGINT NOT NULL,
			kind    VARCHAR(16) NOT NULL,
			version INT NOT NULL DEFAULT 0,
			sentAt  BIGINT NOT NULL DEFAULT 0,
			sends   INT NOT NULL DEFAULT 0,
			PRIMARY KEY (charID, kind)
		)`,
		// The last skill plan the module built for a pilot (skills.go), so the panel can show why a queue looks as it does; the queue itself is core-owned (chrSkillQueue).
		// Rewritten whole on every replan, one row per step.
		`CREATE TABLE IF NOT EXISTS rolebot_skill_plan (
			characterID BIGINT NOT NULL,
			position    INT NOT NULL,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			level       INT NOT NULL DEFAULT 1,
			minutes     DOUBLE NOT NULL DEFAULT 0,
			score       DOUBLE NOT NULL DEFAULT 0,
			why         VARCHAR(255) NOT NULL DEFAULT '',
			kind        VARCHAR(16) NOT NULL DEFAULT '',
			applied     TINYINT NOT NULL DEFAULT 0,
			plannedAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, position)
		)`,
		// One row per planned pilot summarising rolebot_skill_plan for the skill lane's due query
		// (duePlanners): when the plan was built, whether any step went into the queue, and whether the
		// plan is settled (every step applied, or the empty-plan stamp). Written in the same
		// transaction as the plan (storeSkillPlanOnce) and rebuilt from the plan rows at boot
		// (rebuildSkillDue), so it never disagrees with them; the due query then reads one row per
		// pilot instead of grouping every plan step on the server.
		`CREATE TABLE IF NOT EXISTS rolebot_skill_due (
			characterID BIGINT NOT NULL,
			plannedAt   BIGINT NOT NULL DEFAULT 0,
			anyApplied  TINYINT NOT NULL DEFAULT 0,
			settled     TINYINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID)
		)`,
		// Operator overrides on the planner: force = train whether or not the arithmetic asks, ban = never train.
		// Kept sparse, since every row is an argument with the planner.
		`CREATE TABLE IF NOT EXISTS rolebot_skill_prefs (
			characterID BIGINT NOT NULL,
			typeID      BIGINT NOT NULL,
			mode        VARCHAR(8) NOT NULL DEFAULT 'force',
			addedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, typeID)
		)`,
		// Unmet market demand: fit lines a shopping trip wanted that nothing in reach sold, per type (wanted = units asked, asks = trips, last* = most recent).
		// The capability ledger (wants.go): how many enabled rolebots can use each buildable thing, retaken whole each sweep and pruned by stamp.
		`CREATE TABLE IF NOT EXISTS rolebot_capability (
			typeID     BIGINT NOT NULL,
			typeName   VARCHAR(100) NOT NULL DEFAULT '',
			groupID    BIGINT NOT NULL DEFAULT 0,
			categoryID BIGINT NOT NULL DEFAULT 0,
			capable    BIGINT NOT NULL DEFAULT 0,
			users      BIGINT NOT NULL DEFAULT 0,
			metaLevel  BIGINT NOT NULL DEFAULT 0,
			buildable  TINYINT NOT NULL DEFAULT 1,
			updatedAt  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (typeID),
			KEY idx_rbcap_capable (capable),
			KEY idx_rbcap_cat (categoryID, capable)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_market_demand (
			typeID       BIGINT NOT NULL,
			typeName     VARCHAR(100) NOT NULL DEFAULT '',
			wanted       BIGINT NOT NULL DEFAULT 0,
			asks         BIGINT NOT NULL DEFAULT 0,
			lastCharID   BIGINT NOT NULL DEFAULT 0,
			lastSystemID BIGINT NOT NULL DEFAULT 0,
			lastAt       BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (typeID),
			KEY idx_rbdemand_last (lastAt)
		)`,
		// The mineral shortage ledger (minerals.go), per station rather than per type: what a named factory's bill of materials could not cover.
		// Prospectors read it to pick which rock to mine, traders which way to fly the result.
		`CREATE TABLE IF NOT EXISTS rolebot_mineral_demand (
			typeID     BIGINT NOT NULL,
			stationID  BIGINT NOT NULL,
			systemID   BIGINT NOT NULL DEFAULT 0,
			regionID   BIGINT NOT NULL DEFAULT 0,
			typeName   VARCHAR(100) NOT NULL DEFAULT '',
			wanted     BIGINT NOT NULL DEFAULT 0,
			lastCharID BIGINT NOT NULL DEFAULT 0,
			lastAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (typeID, stationID),
			KEY idx_rbmindem_last (lastAt),
			KEY idx_rbmindem_type (typeID, lastAt)
		)`,
		// The belt distress board (beltdistress.go): systems whose belts a miner refused on NPC threat, read by combat pilots and gang formation; a clear settles a row.
		// corpID is part of the key: a cry is addressed to a corporation, and without it the upsert would merge every corporation's view. 0 is the unemployed board.
		`CREATE TABLE IF NOT EXISTS rolebot_belt_distress (
			systemID   BIGINT NOT NULL,
			corpID     BIGINT NOT NULL DEFAULT 0,
			regionID   BIGINT NOT NULL DEFAULT 0,
			security   DOUBLE NOT NULL DEFAULT 0,
			systemName VARCHAR(100) NOT NULL DEFAULT '',
			source     VARCHAR(24) NOT NULL DEFAULT '',
			refusals   BIGINT NOT NULL DEFAULT 0,
			lastCharID BIGINT NOT NULL DEFAULT 0,
			firstAt    BIGINT NOT NULL DEFAULT 0,
			lastAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (systemID, corpID),
			KEY idx_rbdistress_corp (corpID, lastAt),
			KEY idx_rbdistress_region (regionID, lastAt),
			KEY idx_rbdistress_last (lastAt)
		)`,
		// What a miner has refined and not yet listed (mineralbatch.go), keyed per station and pilot: a short stack waits, and its age-out clock runs
		// from the first trip that could not fill a lot, separately in each hangar the miner left one in.
		`CREATE TABLE IF NOT EXISTS rolebot_mineral_hold (
			characterID BIGINT NOT NULL,
			stationID   BIGINT NOT NULL,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			units       BIGINT NOT NULL DEFAULT 0,
			sinceAt     BIGINT NOT NULL DEFAULT 0,
			lastAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, stationID, typeID),
			KEY idx_rbminhold_char (characterID),
			KEY idx_rbminhold_since (sinceAt)
		)`,
		// The shelf (buildbook.go): what one industrialist keeps in stock at one station as standing policy. targetQty is the stock line,
		// source the evidence tier ('operator' is a pin, never pruned), builtQty/soldQty the running history the panel reads.
		`CREATE TABLE IF NOT EXISTS rolebot_industry_shelf (
			characterID BIGINT NOT NULL,
			stationID   BIGINT NOT NULL,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			targetQty   BIGINT NOT NULL DEFAULT 0,
			source      VARCHAR(16) NOT NULL DEFAULT '',
			builtQty    BIGINT NOT NULL DEFAULT 0,
			soldQty     BIGINT NOT NULL DEFAULT 0,
			lastBuiltAt BIGINT NOT NULL DEFAULT 0,
			addedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, stationID, typeID),
			KEY idx_rbshelf_station (stationID),
			KEY idx_rbshelf_built (lastBuiltAt)
		)`,
		// Traffic (traffic.go): what the roster's movement made of the map, as gate arrivals, lane crossings and money changing hands.
		// DOUBLE columns, because they decay (hubDecay): a hub is a hub for what is happening now.
		`CREATE TABLE IF NOT EXISTS rolebot_traffic_systems (
			systemID  BIGINT NOT NULL,
			jumps     DOUBLE NOT NULL DEFAULT 0,
			docks     DOUBLE NOT NULL DEFAULT 0,
			buys      DOUBLE NOT NULL DEFAULT 0,
			sells     DOUBLE NOT NULL DEFAULT 0,
			iskMoved  DOUBLE NOT NULL DEFAULT 0,
			decayedAt BIGINT NOT NULL DEFAULT 0,
			lastAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (systemID)
		)`,
		// Footfall per dock: who is landing where, the one station fact about people rather than architecture (stationscore.go).
		// Decays on the system ledger's half-life.
		`CREATE TABLE IF NOT EXISTS rolebot_traffic_stations (
			stationID BIGINT NOT NULL,
			docks     DOUBLE NOT NULL DEFAULT 0,
			decayedAt BIGINT NOT NULL DEFAULT 0,
			lastAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (stationID)
		)`,
		// Lanes are undirected, stored with the lower system ID first (laneKey): a lane worked one way is still a lane.
		`CREATE TABLE IF NOT EXISTS rolebot_traffic_lanes (
			fromSystemID BIGINT NOT NULL,
			toSystemID   BIGINT NOT NULL,
			jumps        DOUBLE NOT NULL DEFAULT 0,
			hauls        DOUBLE NOT NULL DEFAULT 0,
			iskMoved     DOUBLE NOT NULL DEFAULT 0,
			decayedAt    BIGINT NOT NULL DEFAULT 0,
			lastAt       BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (fromSystemID, toSystemID),
			KEY idx_rblane_to (toSystemID)
		)`,
		// The computed answer, one row per system with a station (a system nobody can dock in cannot be traded in), recomputed on a sweep
		// because every trade planner and the map page read all of it. `clones` is inert: a pilot has two addresses (home, industry base); nothing reads or writes it.
		`CREATE TABLE IF NOT EXISTS rolebot_hub_scores (
			systemID    BIGINT NOT NULL,
			trafficTerm DOUBLE NOT NULL DEFAULT 0,
			laneTerm    DOUBLE NOT NULL DEFAULT 0,
			marketTerm  DOUBLE NOT NULL DEFAULT 0,
			anchorTerm  DOUBLE NOT NULL DEFAULT 0,
			flowTerm    DOUBLE NOT NULL DEFAULT 0,
			stationTerm DOUBLE NOT NULL DEFAULT 0,
			topoTerm    DOUBLE NOT NULL DEFAULT 0,
			secTerm     DOUBLE NOT NULL DEFAULT 0,
			secUnit     DOUBLE NOT NULL DEFAULT 0,
			base        DOUBLE NOT NULL DEFAULT 0,
			shadow      DOUBLE NOT NULL DEFAULT 0,
			shadowBy    BIGINT NOT NULL DEFAULT 0,
			raw         DOUBLE NOT NULL DEFAULT 0,
			score       DOUBLE NOT NULL DEFAULT 0,
			stations    INT NOT NULL DEFAULT 0,
			gates       INT NOT NULL DEFAULT 0,
			orders      BIGINT NOT NULL DEFAULT 0,
			bookISK     DOUBLE NOT NULL DEFAULT 0,
			playerOrders BIGINT NOT NULL DEFAULT 0,
			playerISK   DOUBLE NOT NULL DEFAULT 0,
			homes       INT NOT NULL DEFAULT 0,
			clones      INT NOT NULL DEFAULT 0,
			factories   INT NOT NULL DEFAULT 0,
			school      TINYINT NOT NULL DEFAULT 0,
			schoolCut   DOUBLE NOT NULL DEFAULT 0,
			noise       DOUBLE NOT NULL DEFAULT 0,
			updatedAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (systemID),
			KEY idx_rbhub_score (score)
		)`,
		// The economy epoch: one row, one seed (epoch.go) that the per-epoch stateless draws (fittaste.go) derive from.
		// The economy wipe redraws it, the bot wipe does not, and nothing else writes it.
		`CREATE TABLE IF NOT EXISTS rolebot_epoch (
			id      TINYINT NOT NULL,
			seed    BIGINT  NOT NULL,
			epoch   INT     NOT NULL DEFAULT 1,
			drawnAt BIGINT  NOT NULL DEFAULT 0,
			PRIMARY KEY (id)
		)`,
		// The contest inside a system: one row per station (stationscore.go). The hub score says which system, this says which dock.
		// Indexed on (systemID, raw) because every read is the best dock in a system.
		`CREATE TABLE IF NOT EXISTS rolebot_station_scores (
			stationID    BIGINT NOT NULL,
			systemID     BIGINT NOT NULL DEFAULT 0,
			services     BIGINT NOT NULL DEFAULT 0,
			amenityTerm  DOUBLE NOT NULL DEFAULT 0,
			anchorTerm   DOUBLE NOT NULL DEFAULT 0,
			guestTerm    DOUBLE NOT NULL DEFAULT 0,
			bookTerm     DOUBLE NOT NULL DEFAULT 0,
			homes        INT NOT NULL DEFAULT 0,
			factories    INT NOT NULL DEFAULT 0,
			guests       DOUBLE NOT NULL DEFAULT 0,
			playerOrders BIGINT NOT NULL DEFAULT 0,
			playerISK    DOUBLE NOT NULL DEFAULT 0,
			raw          DOUBLE NOT NULL DEFAULT 0,
			score        DOUBLE NOT NULL DEFAULT 0,
			updatedAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (stationID),
			KEY idx_rbstsc_sys (systemID, raw)
		)`,
		// Measured market liquidity (liquidity.go): decayed ISK and trade count between parties that are not NPC seeders, read forward from market_transactions.
		// The cursor is a typed singleton like rolebot_epoch: the last transactionID taken in.
		`CREATE TABLE IF NOT EXISTS rolebot_station_liquidity (
			stationID BIGINT NOT NULL,
			systemID  BIGINT NOT NULL DEFAULT 0,
			isk       DOUBLE NOT NULL DEFAULT 0,
			trades    DOUBLE NOT NULL DEFAULT 0,
			decayedAt BIGINT NOT NULL DEFAULT 0,
			lastAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (stationID),
			KEY idx_rbliq_sys (systemID)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_liquidity_cursor (
			id       TINYINT NOT NULL,
			lastTxID BIGINT  NOT NULL DEFAULT 0,
			PRIMARY KEY (id)
		)`,
		// Shopper demand at the counter (stationdemand.go): decayed units asked for and got per station and type, with the stops behind them.
		// Written only where a shopper stood, never at a home; the gap is what sellers carry goods toward.
		`CREATE TABLE IF NOT EXISTS rolebot_station_demand (
			stationID BIGINT NOT NULL,
			typeID    BIGINT NOT NULL,
			systemID  BIGINT NOT NULL DEFAULT 0,
			regionID  BIGINT NOT NULL DEFAULT 0,
			asked     DOUBLE NOT NULL DEFAULT 0,
			filled    DOUBLE NOT NULL DEFAULT 0,
			visits    DOUBLE NOT NULL DEFAULT 0,
			decayedAt BIGINT NOT NULL DEFAULT 0,
			lastAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (stationID, typeID),
			KEY idx_rbstdem_type (typeID),
			KEY idx_rbstdem_sys (systemID)
		)`,
		// ISK-flow ledger (iskflow.go): per hour, what the bot economy creates and destroys, read forward from market_journal by a cursor.
		// One row per (hour, wallet kind, iskFlowLines line); isk is the signed sum, inflow the credits alone, since a transfer line's legs cancel in the sum.
		`CREATE TABLE IF NOT EXISTS rolebot_isk_flow (
			hour   INT NOT NULL,
			actor  VARCHAR(4) NOT NULL,
			line   VARCHAR(24) NOT NULL,
			isk    DOUBLE NOT NULL DEFAULT 0,
			inflow DOUBLE NOT NULL DEFAULT 0,
			n      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, actor, line)
		)`,
		// The same flows per corporation and day (a bot's corporation when the row was read, or the corporation wallet),
		// for which corporations capture the faucet.
		`CREATE TABLE IF NOT EXISTS rolebot_isk_flow_corp (
			day           INT NOT NULL,
			corporationID BIGINT NOT NULL,
			line          VARCHAR(24) NOT NULL,
			isk           DOUBLE NOT NULL DEFAULT 0,
			inflow        DOUBLE NOT NULL DEFAULT 0,
			n             BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (day, corporationID, line),
			KEY idx_rbifc_corp (corporationID, day)
		)`,
		// bootstrappedAt is when the ledger started reading: bot journal rows are trimmed to the newest 200 per pilot
		// (db/walletretention.go), so earlier history is partial and the Overview says so.
		`CREATE TABLE IF NOT EXISTS rolebot_isk_flow_cursor (
			id             TINYINT NOT NULL,
			lastRefID      BIGINT NOT NULL DEFAULT 0,
			bootstrappedAt BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (id)
		)`,
		// The roster's market activity from market_transactions by item category, counterparty kind (npc / bot / player) and system.
		// side is the bot's side; a bot-to-bot trade appears once on each side.
		`CREATE TABLE IF NOT EXISTS rolebot_mkt_flow (
			hour       INT NOT NULL,
			categoryID BIGINT NOT NULL,
			side       VARCHAR(4) NOT NULL,
			counter    VARCHAR(6) NOT NULL,
			isk        DOUBLE NOT NULL DEFAULT 0,
			units      BIGINT NOT NULL DEFAULT 0,
			n          BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, categoryID, side, counter)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_mkt_system (
			hour     INT NOT NULL,
			systemID BIGINT NOT NULL,
			side     VARCHAR(4) NOT NULL,
			counter  VARCHAR(6) NOT NULL,
			isk      DOUBLE NOT NULL DEFAULT 0,
			n        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, systemID, side, counter),
			KEY idx_rbms_sys (systemID, hour)
		)`,
		// The same NPC/bot/player split by item group, for the goods breakdown (econgoods.go); category cannot tell ore from ice.
		// Its own cursor row is id 2.
		`CREATE TABLE IF NOT EXISTS rolebot_mkt_group (
			hour    INT NOT NULL,
			groupID BIGINT NOT NULL,
			side    VARCHAR(4) NOT NULL,
			counter VARCHAR(6) NOT NULL,
			isk     DOUBLE NOT NULL DEFAULT 0,
			units   BIGINT NOT NULL DEFAULT 0,
			n       BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (hour, groupID, side, counter)
		)`,
		// The market's history as the trade logic reads it (mktdaily.go): market_transactions folded to (day, station, type) on cursor row 3.
		// Day-first, so the reader's window and the prune are primary-key ranges.
		`CREATE TABLE IF NOT EXISTS rolebot_mkt_daily (
			day       INT NOT NULL,
			stationID BIGINT NOT NULL,
			typeID    BIGINT NOT NULL,
			regionID  BIGINT NOT NULL DEFAULT 0,
			buyUnits  BIGINT NOT NULL DEFAULT 0,
			buyISK    DOUBLE NOT NULL DEFAULT 0,
			buyN      BIGINT NOT NULL DEFAULT 0,
			sellUnits BIGINT NOT NULL DEFAULT 0,
			sellISK   DOUBLE NOT NULL DEFAULT 0,
			minPrice  DOUBLE NOT NULL DEFAULT 0,
			maxPrice  DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (day, stationID, typeID)
		)`,
		// extSellUnits: the sellers' units whose buyer was not a rostered pilot (stationSoldTypes);
		// a roster bid filled by roster sellers is the roster trading with itself.
		`ALTER TABLE rolebot_mkt_daily ADD COLUMN IF NOT EXISTS extSellUnits BIGINT NOT NULL DEFAULT 0`,
		`CREATE TABLE IF NOT EXISTS rolebot_mkt_flow_cursor (
			id       TINYINT NOT NULL,
			lastTxID BIGINT  NOT NULL DEFAULT 0,
			PRIMARY KEY (id)
		)`,
		// The trade book (tradebook.go): every leg of every rolebot trade, one row per movement of goods with the money on it.
		// The trade feed and the flow half of the hub score are built from it; nothing else writes here.
		`CREATE TABLE IF NOT EXISTS rolebot_trades (
			tradeID     BIGINT NOT NULL AUTO_INCREMENT,
			ts          BIGINT NOT NULL,
			characterID BIGINT NOT NULL DEFAULT 0,
			kind        VARCHAR(12) NOT NULL DEFAULT '',
			typeID      BIGINT NOT NULL DEFAULT 0,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			quantity    BIGINT NOT NULL DEFAULT 0,
			price       DOUBLE NOT NULL DEFAULT 0,
			value       DOUBLE NOT NULL DEFAULT 0,
			profit      DOUBLE NOT NULL DEFAULT 0,
			stationID   BIGINT NOT NULL DEFAULT 0,
			systemID    BIGINT NOT NULL DEFAULT 0,
			fromSystemID BIGINT NOT NULL DEFAULT 0,
			orderID     BIGINT NOT NULL DEFAULT 0,
			note        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (tradeID),
			KEY idx_rbtrade_ts (ts),
			KEY idx_rbtrade_char (characterID),
			KEY idx_rbtrade_type (typeID),
			KEY idx_rbtrade_station_kind (stationID, kind, ts, typeID, characterID),
			KEY idx_rbtrade_kind_ts (kind, ts)
		)`,
		// The resale sell-through rollup (tradecapital.go): per day and item category, units sold off a listing against units pulled unsold,
		// folded forward from rolebot_trades by a tradeID cursor.
		`CREATE TABLE IF NOT EXISTS rolebot_trade_calib (
			day        BIGINT NOT NULL,
			categoryID BIGINT NOT NULL,
			sold       DOUBLE NOT NULL DEFAULT 0,
			total      DOUBLE NOT NULL DEFAULT 0,
			events     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (day, categoryID)
		)`,
		// Resting orders a bot has out. market_orders is the truth; this is the memory of why the order exists and what the goods cost,
		// without which a fill cannot be booked as profit.
		`CREATE TABLE IF NOT EXISTS rolebot_orders (
			orderID     BIGINT NOT NULL,
			characterID BIGINT NOT NULL,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			bid         TINYINT NOT NULL DEFAULT 0,
			stationID   BIGINT NOT NULL DEFAULT 0,
			systemID    BIGINT NOT NULL DEFAULT 0,
			price       DOUBLE NOT NULL DEFAULT 0,
			unitCost    DOUBLE NOT NULL DEFAULT 0,
			volEntered  BIGINT NOT NULL DEFAULT 0,
			volRemaining BIGINT NOT NULL DEFAULT 0,
			kind        VARCHAR(12) NOT NULL DEFAULT '',
			placedAt    BIGINT NOT NULL DEFAULT 0,
			lastSeenAt  BIGINT NOT NULL DEFAULT 0,
			closedAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (orderID),
			KEY idx_rborder_char (characterID, closedAt),
			KEY idx_rborder_type (typeID),
			KEY idx_rborder_open (closedAt, bid, kind)
		)`,
		// The module's policy row beside one corporation (corps.go): joinable, tags, how hard the door is. Seeded from defaultCorpPolicy; corpEligibility decides.
		// seeded = 1 means untouched: saveCorpPolicy, the one operator write path, clears it, so changed defaults reach only rows nobody edited.
		`CREATE TABLE IF NOT EXISTS rolebot_corps (
			corporationID BIGINT NOT NULL,
			joinable      TINYINT NOT NULL DEFAULT 0,
			restricted    TINYINT NOT NULL DEFAULT 0,
			tags          VARCHAR(255) NOT NULL DEFAULT '',
			raceRule      VARCHAR(8) NOT NULL DEFAULT '',
			minStanding   DOUBLE NOT NULL DEFAULT 0,
			weight        DOUBLE NOT NULL DEFAULT 1,
			notes         VARCHAR(255) NOT NULL DEFAULT '',
			fleetOreStationID BIGINT NOT NULL DEFAULT 0,
			allegiance    BIGINT NOT NULL DEFAULT 0,
			seeded        TINYINT NOT NULL DEFAULT 1,
			updatedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID),
			KEY idx_rbcorp_joinable (joinable)
		)`,
		// One pilot's recruitment bookkeeping (corpjoin.go): the last and next job-market look and the last move. A review outlives every trip.
		// schoolCorpID is where the pilot goes home to; heldCorpID is where the module last put it, so a different character_.corporationID means a kick.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_reviews (
			characterID  BIGINT NOT NULL,
			lastReviewAt BIGINT NOT NULL DEFAULT 0,
			nextReviewAt BIGINT NOT NULL DEFAULT 0,
			lastMoveAt   BIGINT NOT NULL DEFAULT 0,
			fromCorpID   BIGINT NOT NULL DEFAULT 0,
			schoolCorpID BIGINT NOT NULL DEFAULT 0,
			heldCorpID   BIGINT NOT NULL DEFAULT 0,
			exitDueAt    BIGINT NOT NULL DEFAULT 0,
			moves        INT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID),
			KEY idx_rbcorprev_next (nextReviewAt)
		)`,
		// One rolebot's application to one player corporation (corpapply.go). The application itself lives in chrApplications, which the corp window reads;
		// this row holds when it applied, when the module gives up and the outcome. Keyed on the pair like chrApplications, so a re-application reuses the row.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_applications (
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			appliedAt     BIGINT NOT NULL DEFAULT 0,
			expiresAt     BIGINT NOT NULL DEFAULT 0,
			decidedAt     BIGINT NOT NULL DEFAULT 0,
			state         VARCHAR(16) NOT NULL DEFAULT 'pending',
			score         DOUBLE NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (characterID, corporationID),
			KEY idx_rbapp_state (state),
			KEY idx_rbapp_corp (corporationID, state)
		)`,
		// Chain of command (corprank.go): one row per (corp, pilot), one activity each. manual = 1 is an operator appointment the weekly pass never touches.
		// Bot CEO of a player corporation (corpceo.go): paused stops every power, powersOff (CSV) some, hqMovable = 0 bans moving the HQ; charterJSON is the last charter.
		`CREATE TABLE IF NOT EXISTS rolebot_ceos (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			origin        VARCHAR(16) NOT NULL DEFAULT 'operator',
			installedBy   VARCHAR(64) NOT NULL DEFAULT '',
			installedAt   BIGINT NOT NULL DEFAULT 0,
			paused        TINYINT NOT NULL DEFAULT 0,
			powersOff     VARCHAR(128) NOT NULL DEFAULT '',
			lastThinkAt   BIGINT NOT NULL DEFAULT 0,
			nextThinkAt   BIGINT NOT NULL DEFAULT 0,
			charterJSON   TEXT NULL,
			hqMovable     TINYINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID),
			KEY idx_rbceo_char (characterID),
			KEY idx_rbceo_next (nextThinkAt)
		)`,
		// The NPC chairs (npcchair.go): an NPC corporation run by its own NPC CEO, kept apart from rolebot_ceos, which every reader takes for a bot-run player corporation.
		// securityHeadID is module-side only (no core role); officeReviewAt is its last owner-office pass.
		`CREATE TABLE IF NOT EXISTS rolebot_npc_chairs (
			corporationID  BIGINT NOT NULL,
			ceoID          BIGINT NOT NULL,
			installedAt    BIGINT NOT NULL DEFAULT 0,
			nextThinkAt    BIGINT NOT NULL DEFAULT 0,
			lastSeatPassAt BIGINT NOT NULL DEFAULT 0,
			headReviewAt   BIGINT NOT NULL DEFAULT 0,
			securityHeadID BIGINT NOT NULL DEFAULT 0,
			officeReviewAt BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		`ALTER TABLE rolebot_npc_chairs ADD COLUMN IF NOT EXISTS officeReviewAt BIGINT NOT NULL DEFAULT 0`,
		// Morale (corpmorale.go): one folded value per (pilot, kind), held against the corporation it was earned in (another corporation reads as 0).
		// Kinds starting with "_" are observations.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_morale (
			characterID   BIGINT NOT NULL,
			kind          VARCHAR(20) NOT NULL,
			corporationID BIGINT NOT NULL,
			value         DOUBLE NOT NULL DEFAULT 0,
			at            BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, kind),
			KEY idx_rbcmor_corp (corporationID)
		)`,
		// Every morale event once: the unique key makes a re-run sample harmless. Pruned at 90 days;
		// shadow_exit rows are the would-have-left record the Morale page reads.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_morale_events (
			eventID       BIGINT NOT NULL AUTO_INCREMENT,
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(20) NOT NULL,
			srcKey        VARCHAR(64) NOT NULL,
			weight        DOUBLE NOT NULL DEFAULT 0,
			at            BIGINT NOT NULL,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (eventID),
			UNIQUE KEY uq_rbcmev (characterID, corporationID, kind, srcKey),
			KEY idx_rbcmev_corp (corporationID, kind, at),
			KEY idx_rbcmev_at (at)
		)`,
		// One morale sampling cursor per corporation: startedAt is the first sample (nothing before it is felt), lastPayrollSeen the last payroll run read.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_morale_cursor (
			corporationID   BIGINT NOT NULL,
			startedAt       BIGINT NOT NULL DEFAULT 0,
			sampledAt       BIGINT NOT NULL DEFAULT 0,
			lastPayrollSeen BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// The corporations rolebots founded (corpfound.go): what the cap counts (joined to corporation, so a closed one frees its slot)
		// and what the brand diversity caps read (template, organisation word, adjective, region name).
		`CREATE TABLE IF NOT EXISTS rolebot_founded_corps (
			corporationID BIGINT NOT NULL,
			founderID     BIGINT NOT NULL,
			foundedAt     BIGINT NOT NULL DEFAULT 0,
			regionID      BIGINT NOT NULL DEFAULT 0,
			regionName    VARCHAR(64) NOT NULL DEFAULT '',
			template      VARCHAR(64) NOT NULL DEFAULT '',
			org           VARCHAR(48) NOT NULL DEFAULT '',
			adj           VARCHAR(48) NOT NULL DEFAULT '',
			PRIMARY KEY (corporationID),
			KEY idx_rbfounded_founder (founderID)
		)`,
		// A CEO's standing directive to its members (corpceo_directive.go): how much effort each activity should get;
		// a member's appetite is bent by it in proportion to its obedience.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_directive (
			corporationID BIGINT NOT NULL,
			activity      VARCHAR(16) NOT NULL,
			weight        DOUBLE NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			setAt         BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, activity)
		)`,
		// The CEO ledger (ESP CEO pages): one row per decision a bot CEO made. Pruned by an indexed range delete on (at) in batches,
		// never an ORDER BY over the table.
		`CREATE TABLE IF NOT EXISTS rolebot_ceo_actions (
			actionID      BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			ceoID         BIGINT NOT NULL,
			at            BIGINT NOT NULL,
			power         VARCHAR(16) NOT NULL,
			verb          VARCHAR(16) NOT NULL,
			subjectID     BIGINT NOT NULL DEFAULT 0,
			amount        DOUBLE NOT NULL DEFAULT 0,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (actionID),
			KEY idx_rbceoact_corp (corporationID, at),
			KEY idx_rbceoact_at (at)
		)`,
		// A department head's standing order to its corporation's fleets (corpstaff_dept.go): one live order per (corporation, activity).
		// Gang pickers prefer it while unexpired; a desk's order outside a fleet's band is not taken, the chair's is.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_fleet_orders (
			corporationID BIGINT NOT NULL,
			activity      VARCHAR(24) NOT NULL,
			systemID      BIGINT NOT NULL DEFAULT 0,
			oreNames      VARCHAR(255) NOT NULL DEFAULT '',
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			postedBy      BIGINT NOT NULL DEFAULT 0,
			postedAt      BIGINT NOT NULL DEFAULT 0,
			expiresAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, activity)
		)`,
		// Headhunting (corpstaff_dept.go): a Recruitment Officer's invitation to a rostered pilot.
		// The pilot's corporation review scores the inviting corporation up and the desk takes the invitee on sight.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_invites (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			invitedBy     BIGINT NOT NULL DEFAULT 0,
			invitedAt     BIGINT NOT NULL DEFAULT 0,
			expiresAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID),
			KEY idx_rbinv_char (characterID)
		)`,
		// The wars a bot CEO declared or took on contract (corpceo_war.go): a CEO pays for and retracts only wars here; a human's or the operator's war is theirs.
		// motive is grievance/predation/competition/ally/contract; contractID is set for a merc's war (merc.go). Relations: rolebot_corp_relations (corprelations.go).
		`CREATE TABLE IF NOT EXISTS rolebot_ceo_wars (
			warID         BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			targetOwnerID BIGINT NOT NULL,
			motive        VARCHAR(16) NOT NULL DEFAULT '',
			contractID    BIGINT NOT NULL DEFAULT 0,
			declaredAt    BIGINT NOT NULL DEFAULT 0,
			billsPaid     DOUBLE NOT NULL DEFAULT 0,
			lastScore     DOUBLE NOT NULL DEFAULT 0,
			endedAt       BIGINT NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (warID),
			KEY idx_rbcw_corp (corporationID, endedAt)
		)`,
		// Each side's consent to a war being mutual (corpceo_warmutual.go): the core's mutual flag is one bit either party may flip, this holds both halves.
		// ownerID is the side (a corporation or its alliance); checkedAt the last think that weighed it; withdrawnAt holds a withdrawn side off re-offering.
		`CREATE TABLE IF NOT EXISTS rolebot_war_mutual (
			warID       BIGINT NOT NULL,
			ownerID     BIGINT NOT NULL,
			consent     TINYINT NOT NULL DEFAULT 0,
			since       BIGINT NOT NULL DEFAULT 0,
			checkedAt   BIGINT NOT NULL DEFAULT 0,
			withdrawnAt BIGINT NOT NULL DEFAULT 0,
			reason      VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (warID, ownerID)
		)`,
		// The mercenary market (merc.go). A job is a client corporation's posting: offense (the merc declares on target) or defense
		// (counter-declares on the aggressor of warID). budget is the client's private walk-away.
		`CREATE TABLE IF NOT EXISTS rolebot_merc_jobs (
			jobID         BIGINT NOT NULL AUTO_INCREMENT,
			clientCorpID  BIGINT NOT NULL,
			kind          VARCHAR(8) NOT NULL,
			targetOwnerID BIGINT NOT NULL,
			targetCorpID  BIGINT NOT NULL DEFAULT 0,
			warID         BIGINT NOT NULL DEFAULT 0,
			motive        VARCHAR(16) NOT NULL DEFAULT '',
			weeks         INT NOT NULL DEFAULT 2,
			budget        DOUBLE NOT NULL DEFAULT 0,
			valueWeekly   DOUBLE NOT NULL DEFAULT 0,
			openWeekly    DOUBLE NOT NULL DEFAULT 0,
			round         INT NOT NULL DEFAULT 0,
			status        VARCHAR(12) NOT NULL DEFAULT 'open',
			postedAt      BIGINT NOT NULL DEFAULT 0,
			quoteUntil    BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (jobID),
			KEY idx_rbmj_status (status, postedAt),
			KEY idx_rbmj_client (clientCorpID, status)
		)`,
		// One mercenary's quote on a job: its opening weekly ask, its private
		// walk-away and how stubbornly it concedes (merc.go concession).
		`CREATE TABLE IF NOT EXISTS rolebot_merc_bids (
			jobID       BIGINT NOT NULL,
			mercCorpID  BIGINT NOT NULL,
			askWeekly   DOUBLE NOT NULL DEFAULT 0,
			walkWeekly  DOUBLE NOT NULL DEFAULT 0,
			concedeExp  DOUBLE NOT NULL DEFAULT 1,
			bonusRate   DOUBLE NOT NULL DEFAULT 0,
			pilots      INT NOT NULL DEFAULT 0,
			credibility DOUBLE NOT NULL DEFAULT 1,
			lastAsk     DOUBLE NOT NULL DEFAULT 0,
			lastOffer   DOUBLE NOT NULL DEFAULT 0,
			status      VARCHAR(12) NOT NULL DEFAULT 'quoted',
			quotedAt    BIGINT NOT NULL DEFAULT 0,
			why         VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (jobID, mercCorpID),
			KEY idx_rbmb_merc (mercCorpID, status)
		)`,
		// A hired merc: what was agreed, what has been paid, and why it ended.
		`CREATE TABLE IF NOT EXISTS rolebot_merc_contracts (
			contractID     BIGINT NOT NULL AUTO_INCREMENT,
			jobID          BIGINT NOT NULL,
			clientCorpID   BIGINT NOT NULL,
			mercCorpID     BIGINT NOT NULL,
			kind           VARCHAR(8) NOT NULL,
			targetOwnerID  BIGINT NOT NULL,
			warID          BIGINT NOT NULL DEFAULT 0,
			retainer       DOUBLE NOT NULL DEFAULT 0,
			weekly         DOUBLE NOT NULL DEFAULT 0,
			bonusRate      DOUBLE NOT NULL DEFAULT 0,
			bonusWeeklyCap DOUBLE NOT NULL DEFAULT 0,
			weeks          INT NOT NULL DEFAULT 0,
			startAt        BIGINT NOT NULL DEFAULT 0,
			endAt          BIGINT NOT NULL DEFAULT 0,
			lastSettledAt  BIGINT NOT NULL DEFAULT 0,
			lastFieldedAt  BIGINT NOT NULL DEFAULT 0,
			paidTotal      DOUBLE NOT NULL DEFAULT 0,
			bonusPaid      DOUBLE NOT NULL DEFAULT 0,
			iskDestroyed   DOUBLE NOT NULL DEFAULT 0,
			status         VARCHAR(12) NOT NULL DEFAULT 'active',
			endedAt        BIGINT NOT NULL DEFAULT 0,
			endReason      VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (contractID),
			KEY idx_rbmc_status (status),
			KEY idx_rbmc_merc (mercCorpID, status),
			KEY idx_rbmc_client (clientCorpID, status)
		)`,
		// An alliance owns no wallet or hangar here, so its ledger is the tithe: what the executor fronted and what each member owes (pending, paid, refused, lapsed).
		// Keyed per row, never per alliance: rolebot_corp_commit is UNIQUE(refKind, refID) across every corporation, so an alliance-wide key would collide between members.
		`CREATE TABLE IF NOT EXISTS rolebot_alliance_ledger (
			titheID        BIGINT NOT NULL AUTO_INCREMENT,
			allianceID     BIGINT NOT NULL,
			executorCorpID BIGINT NOT NULL,
			debtorCorpID   BIGINT NOT NULL,
			kind           VARCHAR(8) NOT NULL DEFAULT 'war',
			refID          BIGINT NOT NULL DEFAULT 0,
			amount         DOUBLE NOT NULL DEFAULT 0,
			requestedAt    BIGINT NOT NULL DEFAULT 0,
			dueAt          BIGINT NOT NULL DEFAULT 0,
			paidAt         BIGINT NOT NULL DEFAULT 0,
			status         VARCHAR(8) NOT NULL DEFAULT 'pending',
			reason         VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (titheID),
			KEY idx_rbal_alliance (allianceID, status),
			KEY idx_rbal_debtor (debtorCorpID, status)
		)`,
		// Requests put to the alliance: a member's war grievance, a defend call when a tower is reinforced, a human executor's order from the portal (posterID).
		// The executor's think takes them; result is the verdict's words.
		`CREATE TABLE IF NOT EXISTS rolebot_alliance_requests (
			requestID  BIGINT NOT NULL AUTO_INCREMENT,
			allianceID BIGINT NOT NULL,
			fromCorpID BIGINT NOT NULL,
			posterID   BIGINT NOT NULL DEFAULT 0,
			kind       VARCHAR(8) NOT NULL,
			targetID   BIGINT NOT NULL DEFAULT 0,
			systemID   BIGINT NOT NULL DEFAULT 0,
			payload    TEXT NULL,
			note       VARCHAR(255) NOT NULL DEFAULT '',
			postedAt   BIGINT NOT NULL DEFAULT 0,
			expiresAt  BIGINT NOT NULL DEFAULT 0,
			status     VARCHAR(8) NOT NULL DEFAULT 'pending',
			result     VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (requestID),
			KEY idx_rbar_alliance (allianceID, status, kind)
		)`,
		// Help calls (gangcalls.go): a fleet in trouble, or a corporation's call to arms, asking kin fleets nearby to converge.
		// gangID 0 is the corporation's own call (a siege or an alliance defend).
		`CREATE TABLE IF NOT EXISTS rolebot_site_runs (
			runID      BIGINT NOT NULL AUTO_INCREMENT,
			siteID     BIGINT NOT NULL,
			dungeonVID BIGINT NOT NULL DEFAULT 0,
			systemID   BIGINT NOT NULL DEFAULT 0,
			charID     BIGINT NOT NULL,
			gangID     BIGINT NOT NULL DEFAULT 0,
			tier       VARCHAR(8) NOT NULL DEFAULT '',
			startedAt  BIGINT NOT NULL DEFAULT 0,
			endedAt    BIGINT NOT NULL DEFAULT 0,
			outcome    VARCHAR(8) NOT NULL DEFAULT '',
			kills      INT NOT NULL DEFAULT 0,
			PRIMARY KEY (runID),
			KEY idx_rsr_vid (dungeonVID, outcome),
			KEY idx_rsr_ended (endedAt)
		)`,
		// Agent missions (missions.go): one row per offer declined, mission quit or mission turned in.
		// note carries the reason for a decline or a quit, so a template's author can see why bots skip it.
		`CREATE TABLE IF NOT EXISTS rolebot_mission_runs (
			runID      BIGINT NOT NULL AUTO_INCREMENT,
			charID     BIGINT NOT NULL,
			missionID  BIGINT NOT NULL DEFAULT 0,
			templateID BIGINT NOT NULL DEFAULT 0,
			agentID    BIGINT NOT NULL DEFAULT 0,
			kind       VARCHAR(12) NOT NULL DEFAULT '',
			tier       VARCHAR(8) NOT NULL DEFAULT '',
			startedAt  BIGINT NOT NULL DEFAULT 0,
			endedAt    BIGINT NOT NULL DEFAULT 0,
			outcome    VARCHAR(12) NOT NULL DEFAULT '',
			note       VARCHAR(255) NOT NULL DEFAULT '',
			rewardISK  BIGINT NOT NULL DEFAULT 0,
			rewardLP   INT NOT NULL DEFAULT 0,
			PRIMARY KEY (runID),
			KEY idx_rmr_template (templateID, outcome),
			KEY idx_rmr_char (charID),
			KEY idx_rmr_ended (endedAt)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_fleet_help (
			callID     BIGINT NOT NULL AUTO_INCREMENT,
			gangID     BIGINT NOT NULL DEFAULT 0,
			corpID     BIGINT NOT NULL,
			allianceID BIGINT NOT NULL DEFAULT 0,
			systemID   BIGINT NOT NULL,
			kind       VARCHAR(12) NOT NULL,
			priority   INT NOT NULL DEFAULT 0,
			need       INT NOT NULL DEFAULT 0,
			note       VARCHAR(255) NOT NULL DEFAULT '',
			postedAt   BIGINT NOT NULL DEFAULT 0,
			expiresAt  BIGINT NOT NULL DEFAULT 0,
			closedAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (callID),
			KEY idx_rbfh_open (closedAt, corpID, expiresAt),
			KEY idx_rbfh_gang (gangID, kind, closedAt)
		)`,
		// The alliance's own book: every decision taken in its name and by which member corporation, for the panel feed.
		// Member CEOs' own lines stay in rolebot_ceo_actions.
		`CREATE TABLE IF NOT EXISTS rolebot_alliance_book (
			entryID     BIGINT NOT NULL AUTO_INCREMENT,
			allianceID  BIGINT NOT NULL,
			actorCorpID BIGINT NOT NULL DEFAULT 0,
			at          BIGINT NOT NULL DEFAULT 0,
			verb        VARCHAR(16) NOT NULL,
			subjectID   BIGINT NOT NULL DEFAULT 0,
			amount      DOUBLE NOT NULL DEFAULT 0,
			detail      VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (entryID),
			KEY idx_rbab_alliance (allianceID, at)
		)`,
		// The rungs of the alliance's ladder it has reached, first time each (allianceladder.go).
		`CREATE TABLE IF NOT EXISTS rolebot_alliance_milestones (
			allianceID BIGINT NOT NULL,
			milestone  VARCHAR(16) NOT NULL,
			reachedAt  BIGINT NOT NULL DEFAULT 0,
			systemID   BIGINT NOT NULL DEFAULT 0,
			stationID  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (allianceID, milestone)
		)`,
		// Systems the executor named for the alliance (alliancesov.go): sov targets its members' Infrastructure departments plant towards,
		// and staging systems its fleets muster in.
		`CREATE TABLE IF NOT EXISTS rolebot_alliance_targets (
			allianceID BIGINT NOT NULL,
			systemID   BIGINT NOT NULL,
			kind       VARCHAR(8) NOT NULL DEFAULT 'sov',
			score      DOUBLE NOT NULL DEFAULT 0,
			setAt      BIGINT NOT NULL DEFAULT 0,
			reason     VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (allianceID, systemID)
		)`,
		// A tower the alliance pledged to fund in one of its sov systems (alliancesovpledge.go): the builder, the system, the
		// sum raised among the members (rolebot_alliance_ledger kind 'sovtower', refID = pledgeID) and where it stands.
		`CREATE TABLE IF NOT EXISTS rolebot_alliance_sovpledge (
			pledgeID      BIGINT NOT NULL AUTO_INCREMENT,
			allianceID    BIGINT NOT NULL,
			builderCorpID BIGINT NOT NULL,
			systemID      BIGINT NOT NULL,
			moonID        BIGINT NOT NULL DEFAULT 0,
			total         DOUBLE NOT NULL DEFAULT 0,
			shortfall     DOUBLE NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			planID        BIGINT NOT NULL DEFAULT 0,
			createdAt     BIGINT NOT NULL DEFAULT 0,
			expiresAt     BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (pledgeID),
			KEY idx_rbasp_alliance (allianceID, status),
			KEY idx_rbasp_builder (builderCorpID, status)
		)`,
		// Which alliances a bot CEO founded (alliancefound.go): the rows the founding cap counts; a dissolved alliance frees its slot.
		`CREATE TABLE IF NOT EXISTS rolebot_founded_alliances (
			allianceID    BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			foundedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (allianceID)
		)`,
		// Why a bot founded it: 'sov' for an alliance founded to hold 0.0 (alliancefound.go, frontier.go), '' otherwise.
		`ALTER TABLE rolebot_founded_alliances ADD COLUMN IF NOT EXISTS motive VARCHAR(8) NOT NULL DEFAULT ''`,
		// An alliance war is booked by the executor corporation with the alliance as its owner, so RetractWar and the bill routing
		// know whose war it is. 0 = the corporation's own.
		`ALTER TABLE rolebot_ceo_wars ADD COLUMN IF NOT EXISTS ownerID BIGINT NOT NULL DEFAULT 0`,
		// Which of a corporation's eight title slots the module owns (corptitles.go): a slot is claimed only while it carries
		// the default "Title N" name or a name the module wrote; a slot a human renamed is never touched.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_titles (
			corporationID BIGINT NOT NULL,
			titleID       BIGINT NOT NULL,
			kind          VARCHAR(16) NOT NULL DEFAULT '',
			name          VARCHAR(100) NOT NULL DEFAULT '',
			PRIMARY KEY (corporationID, titleID)
		)`,
		// The operator's names for a corporation's positions (corptitlevocab.go), one per title key, set on the corporation page.
		// They beat the vocabulary, never a title a human typed.
		`CREATE TABLE IF NOT EXISTS rolebot_title_overrides (
			corporationID BIGINT NOT NULL,
			titleKey      VARCHAR(32) NOT NULL,
			name          VARCHAR(100) NOT NULL,
			setAt         BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, titleKey)
		)`,
		// What the module last wrote into a member's character_.title and the position it stood for (corptitles.go).
		// A member whose text still reads that is the module's to rename; anything else is a title somebody typed.
		`CREATE TABLE IF NOT EXISTS rolebot_title_text (
			characterID   BIGINT NOT NULL PRIMARY KEY,
			corporationID BIGINT NOT NULL DEFAULT 0,
			titleKey      VARCHAR(32) NOT NULL DEFAULT '',
			text          VARCHAR(100) NOT NULL DEFAULT '',
			at            BIGINT NOT NULL DEFAULT 0
		)`,
		// A managed corporation's books by line of business (corpledger.go): one signed row per flow the module moved on its behalf (+income, -cost).
		// The CEO page's P&L reads it. Pruned by an indexed range delete on (at).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_ledger (
			entryID       BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			at            BIGINT NOT NULL,
			line          VARCHAR(16) NOT NULL,
			amount        DOUBLE NOT NULL DEFAULT 0,
			counterID     BIGINT NOT NULL DEFAULT 0,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (entryID),
			KEY idx_rbcl_corp (corporationID, at),
			KEY idx_rbcl_at (at)
		)`,
		// Each holder's cost basis in a corporation's shares (corpshares.go): contributed is what it paid (ISK or a print in kind), paidOut what came back
		// (dividends, buybacks). The holding itself is crpcharshares. Rows from before shares existed were donations, converted to shares at par once.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_equity (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			contributed   DOUBLE NOT NULL DEFAULT 0,
			paidOut       DOUBLE NOT NULL DEFAULT 0,
			lastAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// A managed corporation's share desk (corpshares.go): the premium the CEO asks over fair value, this week's sales and buybacks
		// (what the premium adapts to), demand the desk could not fill, and whether the founder's stake was granted.
		`CREATE TABLE IF NOT EXISTS rolebot_share_state (
			corporationID  BIGINT NOT NULL,
			premium        DOUBLE NOT NULL DEFAULT 0,
			weekStart      BIGINT NOT NULL DEFAULT 0,
			weekSold       BIGINT NOT NULL DEFAULT 0,
			weekRedeemed   BIGINT NOT NULL DEFAULT 0,
			unmetISK       DOUBLE NOT NULL DEFAULT 0,
			founderGranted TINYINT NOT NULL DEFAULT 0,
			lastDividendAt BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// One day's book value per share per managed corporation: what a holder reads growth off (corpshares.go shareYield).
		`CREATE TABLE IF NOT EXISTS rolebot_share_book (
			corporationID BIGINT NOT NULL,
			day           BIGINT NOT NULL,
			book          DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, day)
		)`,
		// Shares a holder sold back to a corporation that could not pay in full at the time (corpshares.go). The shares are already in the treasury;
		// owed is what remains, paid first in first out as the buyback allowance frees up.
		`CREATE TABLE IF NOT EXISTS rolebot_share_redemptions (
			redemptionID  BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			holderID      BIGINT NOT NULL,
			shares        BIGINT NOT NULL DEFAULT 0,
			price         DOUBLE NOT NULL DEFAULT 0,
			owed          DOUBLE NOT NULL DEFAULT 0,
			createdAt     BIGINT NOT NULL DEFAULT 0,
			paidAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (redemptionID),
			KEY idx_rbsr_corp (corporationID, paidAt)
		)`,
		// Market orders a CEO placed on its corporation's account (corpstock.go). The order lives in market_orders; this row lets the business pass
		// book a fill as sales (volSeen is the last volRemaining seen) and reprice an order that has sat.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_orders (
			orderID       BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			stationID     BIGINT NOT NULL,
			typeID        BIGINT NOT NULL,
			bid           TINYINT NOT NULL DEFAULT 0,
			price         DOUBLE NOT NULL DEFAULT 0,
			volSeen       BIGINT NOT NULL DEFAULT 0,
			placedAt      BIGINT NOT NULL DEFAULT 0,
			repricedAt    BIGINT NOT NULL DEFAULT 0,
			kind          VARCHAR(8) NOT NULL DEFAULT 'stock',
			placedBy      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (orderID),
			KEY idx_rbco_corp (corporationID)
		)`,
		// The industry desk (docs/CORPS.md section 10): one row per managed corporation with the ore desk's build share and why (oredesk.go),
		// who runs its factories (corpdirector.go), and the last Production Board mail (corpwork.go).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_policy (
			corporationID BIGINT NOT NULL,
			oreShare      DOUBLE NOT NULL DEFAULT 0,
			oreTarget     DOUBLE NOT NULL DEFAULT 0,
			oreAt         BIGINT NOT NULL DEFAULT 0,
			oreWhy        VARCHAR(255) NOT NULL DEFAULT '',
			directorID    BIGINT NOT NULL DEFAULT 0,
			directorSince BIGINT NOT NULL DEFAULT 0,
			deputyID      BIGINT NOT NULL DEFAULT 0,
			salaryAt      BIGINT NOT NULL DEFAULT 0,
			boardHash     VARCHAR(40) NOT NULL DEFAULT '',
			boardMailAt   BIGINT NOT NULL DEFAULT 0,
			updatedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// When the ore desk started holding a type at an office rather than selling it. Anything held past corpOreHoldDays is sold regardless:
		// the desk may hold ore for production, never strand it.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_orehold (
			corporationID BIGINT NOT NULL,
			stationID     BIGINT NOT NULL,
			typeID        BIGINT NOT NULL,
			keepQty       BIGINT NOT NULL DEFAULT 0,
			heldSince     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, stationID, typeID)
		)`,
		// Money the corporation has promised and not yet paid: wages on posted work, buyback escrow, purchase advances.
		// Free capital is the wallet less the reserve less every open row's (amount - spent).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_commit (
			commitID      BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			refKind       VARCHAR(16) NOT NULL,
			refID         BIGINT NOT NULL,
			amount        DOUBLE NOT NULL DEFAULT 0,
			spent         DOUBLE NOT NULL DEFAULT 0,
			openedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (commitID),
			UNIQUE KEY uq_rbcc_ref (refKind, refID),
			KEY idx_rbcc_corp (corporationID, closedAt)
		)`,
		// The work board (corpwork.go): paid work the production power posts and any member, client or module, carries out.
		// kind is build, me, pe or refine; purpose says why (srp, store, market).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_work (
			workID        BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(8) NOT NULL,
			activityID    INT NOT NULL DEFAULT 0,
			bpTypeID      BIGINT NOT NULL DEFAULT 0,
			productTypeID BIGINT NOT NULL DEFAULT 0,
			stationID     BIGINT NOT NULL DEFAULT 0,
			runsWanted    INT NOT NULL DEFAULT 0,
			runsDone      INT NOT NULL DEFAULT 0,
			wagePerRun    DOUBLE NOT NULL DEFAULT 0,
			purpose       VARCHAR(8) NOT NULL DEFAULT 'market',
			priority      INT NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			postedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (workID),
			KEY idx_rbcw_corp (corporationID, status),
			KEY idx_rbcw_station (status, stationID)
		)`,
		// A bot's claim on runs of one work order, so two builders do not take the last batch. Stale after corpWorkClaimSec unless a job was installed against it.
		// Humans never claim: the wage sweep pays whatever arrives while runs remain.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_work_claims (
			workID      BIGINT NOT NULL,
			characterID BIGINT NOT NULL,
			runs        INT NOT NULL DEFAULT 0,
			jobID       BIGINT NOT NULL DEFAULT 0,
			claimedAt   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (workID, characterID, jobID),
			KEY idx_rbcwc_char (characterID)
		)`,
		// Every blueprint a pilot (or a corporation through its broker) put on escrow, copy or original (bpclistings.go). It adds what the escrow row lacks:
		// the print's type, copy flag, runs and research, which a researcher uses to count its own copy stock.
		`CREATE TABLE IF NOT EXISTS rolebot_bpc_listings (
			escrowID      BIGINT NOT NULL PRIMARY KEY,
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL DEFAULT 0,
			bpTypeID      BIGINT NOT NULL,
			isCopy        TINYINT NOT NULL DEFAULT 0,
			runs          INT NOT NULL DEFAULT 0,
			me            INT NOT NULL DEFAULT 0,
			pe            INT NOT NULL DEFAULT 0,
			price         DOUBLE NOT NULL DEFAULT 0,
			listedAt      BIGINT NOT NULL DEFAULT 0,
			KEY idx_rbbl_char (characterID),
			KEY idx_rbbl_type (bpTypeID, isCopy)
		)`,
		// A library print the CEO put up for sale (corpprintsale.go): marked, listed on escrow by a broker at the factory, then sold, returned to the shelf, or defaulted.
		// One row per print; a returned print is marked again on the same row.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_print_sales (
			itemID        BIGINT NOT NULL PRIMARY KEY,
			corporationID BIGINT NOT NULL,
			bpTypeID      BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(16) NOT NULL DEFAULT 'wanted',
			brokerID      BIGINT NOT NULL DEFAULT 0,
			escrowID      BIGINT NOT NULL DEFAULT 0,
			price         DOUBLE NOT NULL DEFAULT 0,
			markedAt      BIGINT NOT NULL DEFAULT 0,
			listedAt      BIGINT NOT NULL DEFAULT 0,
			returnedAt    BIGINT NOT NULL DEFAULT 0,
			KEY idx_rbcps_corp (corporationID, status),
			KEY idx_rbcps_status (status)
		)`,
		// The office a marked copy is to be sold from (corpprintsale.go): where its type sells among the stations members work at.
		// 0 = a row marked before the copy service, listed wherever its copy sits.
		`ALTER TABLE rolebot_corp_print_sales ADD COLUMN IF NOT EXISTS saleStationID BIGINT NOT NULL DEFAULT 0`,
		// One row per delivered corporation job the wage sweep has read, paid or not (workID 0 = off the board).
		// The row is the idempotence: a job is never paid twice.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_wages (
			jobID         BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			workID        BIGINT NOT NULL DEFAULT 0,
			characterID   BIGINT NOT NULL DEFAULT 0,
			runs          INT NOT NULL DEFAULT 0,
			wage          DOUBLE NOT NULL DEFAULT 0,
			paidAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (jobID),
			KEY idx_rbcwg_corp (corporationID, paidAt),
			KEY idx_rbcwg_char (characterID, paidAt)
		)`,
		// Labour a member did for a bot-run corporation (corppayroll.go): a delivered job, haul, survey, carried print or assignment. No ISK moves on write;
		// the row is merit evidence (corpmerit.go reads it on its own cursor) and the record the weekly payroll stamps paid. credit weighs work against work.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_labour (
			entryID       BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			at            BIGINT NOT NULL,
			kind          VARCHAR(24) NOT NULL DEFAULT '',
			credit        DOUBLE NOT NULL DEFAULT 0,
			detail        VARCHAR(255) NOT NULL DEFAULT '',
			paidAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (entryID),
			KEY idx_rbclab_corp (corporationID, entryID),
			KEY idx_rbclab_member (corporationID, characterID, at)
		)`,
		// One row per bot-run corporation: when its weekly payroll last ran
		// and what it paid (corppayroll.go).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_payroll (
			corporationID BIGINT NOT NULL,
			lastPayrollAt BIGINT NOT NULL DEFAULT 0,
			lastProfit    DOUBLE NOT NULL DEFAULT 0,
			lastPot       DOUBLE NOT NULL DEFAULT 0,
			lastPaid      INT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// When the next payroll is due (corppayroll.go). 0 = a row from
		// before the column, re-opened on its next read.
		`ALTER TABLE rolebot_corp_payroll ADD COLUMN IF NOT EXISTS nextPayAt BIGINT NOT NULL DEFAULT 0`,
		// When the CEO last ran its weekly review (roles, estate and share-book powers; corpceo.go reviewDue). 0 = never, due now.
		`ALTER TABLE rolebot_ceos ADD COLUMN IF NOT EXISTS lastReviewAt BIGINT NOT NULL DEFAULT 0`,
		// The industry role bits the module granted each member (corpdirector.go). Only these are ever taken away again,
		// so a role a human officer gave by hand (Trader, a hangar take) is never stripped by the industry pass.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_grants (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			mask          BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// A builder benched from corporation work until a time, for running jobs the board never asked for on the corporation's materials.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_bench (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			untilAt       BIGINT NOT NULL DEFAULT 0,
			why           VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (corporationID, characterID)
		)`,
		// The blueprint library (corplibrary.go): every print the corporation holds in its offices, reconciled against entity each think.
		// source is donation, purchase or found.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_library (
			itemID        BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			bpTypeID      BIGINT NOT NULL,
			stationID     BIGINT NOT NULL DEFAULT 0,
			source        VARCHAR(12) NOT NULL DEFAULT 'found',
			cost          DOUBLE NOT NULL DEFAULT 0,
			donorID       BIGINT NOT NULL DEFAULT 0,
			acquiredAt    BIGINT NOT NULL DEFAULT 0,
			lastSeenAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (itemID),
			KEY idx_rbclib_corp (corporationID, bpTypeID)
		)`,
		// A purchase order: corporation money advanced to a member to buy
		// one blueprint and deliver it to the library.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_po (
			poID          BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			bpTypeID      BIGINT NOT NULL,
			stationID     BIGINT NOT NULL DEFAULT 0,
			advance       DOUBLE NOT NULL DEFAULT 0,
			spent         DOUBLE NOT NULL DEFAULT 0,
			status        VARCHAR(10) NOT NULL DEFAULT 'open',
			itemID        BIGINT NOT NULL DEFAULT 0,
			openedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (poID),
			KEY idx_rbcpo_corp (corporationID, status),
			KEY idx_rbcpo_char (characterID, status)
		)`,
		// The print fund (corpfund.go): saving toward a hull print the one-think budget cannot reach ("print") or one run's bill of a big hull held ("bill").
		// saved is mirrored into a rolebot_corp_commit row (refKind "fund") that shields it from the other desks; no ISK moves.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_fund (
			fundID        BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			bpTypeID      BIGINT NOT NULL,
			kind          VARCHAR(8) NOT NULL DEFAULT 'print',
			target        DOUBLE NOT NULL DEFAULT 0,
			saved         DOUBLE NOT NULL DEFAULT 0,
			openedAt      BIGINT NOT NULL DEFAULT 0,
			lastAddAt     BIGINT NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			status        VARCHAR(10) NOT NULL DEFAULT 'saving',
			poID          BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (fundID),
			KEY idx_rbcf_corp (corporationID, kind, status),
			KEY idx_rbcf_po (poID)
		)`,
		// A capital program (corpprogram.go): one big hull a corporation (or a consortium around it) builds from components, stage by stage:
		// saving for the hull print and component prints, building the parts and the hull, handing it to a pilot.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_program (
			programID     BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			allianceID    BIGINT NOT NULL DEFAULT 0,
			kind          VARCHAR(12) NOT NULL DEFAULT 'freighter',
			hullTypeID    BIGINT NOT NULL,
			hullBPTypeID  BIGINT NOT NULL,
			yardStationID BIGINT NOT NULL DEFAULT 0,
			stage         VARCHAR(12) NOT NULL DEFAULT 'saving',
			builderID     BIGINT NOT NULL DEFAULT 0,
			pilotID       BIGINT NOT NULL DEFAULT 0,
			hullItemID    BIGINT NOT NULL DEFAULT 0,
			openedAt      BIGINT NOT NULL DEFAULT 0,
			updatedAt     BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (programID),
			KEY idx_rbcprog_corp (corporationID, stage)
		)`,
		// The program's component lines: what the hull needs of each, who supplies it (the yard itself or a consortium member),
		// and how much has reached the yard's Materials division.
		`CREATE TABLE IF NOT EXISTS rolebot_program_parts (
			programID       BIGINT NOT NULL,
			componentTypeID BIGINT NOT NULL,
			bpTypeID        BIGINT NOT NULL DEFAULT 0,
			supplierCorpID  BIGINT NOT NULL DEFAULT 0,
			needed          BIGINT NOT NULL DEFAULT 0,
			delivered       BIGINT NOT NULL DEFAULT 0,
			unitPrice       DOUBLE NOT NULL DEFAULT 0,
			status          VARCHAR(10) NOT NULL DEFAULT 'open',
			PRIMARY KEY (programID, componentTypeID)
		)`,
		// A move of the industry desk's stock from an office that stopped being the factory to the one that is (corpfactorymove.go): one row per old office,
		// carried a hold at a time by the director or deputy. carrying is the JSON list of what is in the carrier's hold, with the division each item came out of.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_moves (
			corporationID BIGINT NOT NULL,
			fromOfficeID  BIGINT NOT NULL,
			fromStationID BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			carrierID     BIGINT NOT NULL DEFAULT 0,
			claimedAt     BIGINT NOT NULL DEFAULT 0,
			carrying      TEXT NULL,
			trips         INT NOT NULL DEFAULT 0,
			openedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (corporationID, fromOfficeID),
			KEY idx_rbcmove_status (status)
		)`,
		// A bot-run corporation's offices (corpoffices.go): crpoffices says one exists, this says what it is for (purposes, a bitmask officeHQ..officeBranch)
		// and whether it is being dropped (state 'dropping'). purposes is re-derived each estate think, OR'd with claimed; a row with no crpoffices row is swept.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_offices (
			corporationID BIGINT NOT NULL,
			stationID     BIGINT NOT NULL,
			purposes      INT NOT NULL DEFAULT 0,
			claimed       INT NOT NULL DEFAULT 0,
			state         VARCHAR(8) NOT NULL DEFAULT 'open',
			rentedAt      BIGINT NOT NULL DEFAULT 0,
			droppingAt    BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (corporationID, stationID),
			KEY idx_rbcoff_state (state)
		)`,
		// Who set a member's Base (chrCorporationRoles.baseID stays the truth) and when (corpposting.go): 'ceo' for the posting pass, which may move it again;
		// 'human' (Edit Member) and 'member' (own pick) are never overridden. arm is the arm the posting was made for.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_postings (
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			stationID     BIGINT NOT NULL,
			setBy         VARCHAR(8) NOT NULL DEFAULT 'ceo',
			arm           VARCHAR(16) NOT NULL DEFAULT '',
			postedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID),
			KEY idx_rbcpost_corp (corporationID)
		)`,
		// A corporation's front or capital (corpfront.go): the office its field arms (kind 'front') or all its arms (kind 'capital')
		// stage out to, the 0.0 system that called for it, since when, and the purpose bits the pass claimed there (taken back on retreat).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_fronts (
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(8) NOT NULL DEFAULT 'front',
			stationID     BIGINT NOT NULL,
			systemID      BIGINT NOT NULL DEFAULT 0,
			claimedBits   INT NOT NULL DEFAULT 0,
			since         BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// The doors a capital keeps (stationpolicy.go): the policy last written and when.
		`ALTER TABLE rolebot_corp_fronts ADD COLUMN IF NOT EXISTS policy VARCHAR(10) NOT NULL DEFAULT ''`,
		`ALTER TABLE rolebot_corp_fronts ADD COLUMN IF NOT EXISTS policyAt BIGINT NOT NULL DEFAULT 0`,
		// The department job roles the arm pass holds for a capsuleer at its base (corpbaseroles.go), so only those bits are ever taken back.
		// A corporation's war chest (corpdivisions.go): the hangar division each borrowed purpose (armory, yard, spoils) lives in; purposes may share one.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_divisions (
			corporationID BIGINT NOT NULL,
			purpose       VARCHAR(12) NOT NULL,
			division      TINYINT NOT NULL,
			assignedAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, purpose)
		)`,
		// The hangar a chair chose for a war-chest purpose (corphangars.go): division 1..7, or -1 for none. No row is the allocator's to place.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_div_pins (
			corporationID BIGINT NOT NULL,
			purpose       VARCHAR(12) NOT NULL,
			division      TINYINT NOT NULL,
			setBy         BIGINT NOT NULL DEFAULT 0,
			since         BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, purpose)
		)`,
		// The Armory and the Fleet Yard (corparmory.go): one row per kit the quartermaster planned or assembled.
		// shipItemID is the assembled corporation hull (0 while planned); status planned | ready | issued.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_kit (
			kitID         BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(8) NOT NULL,
			seatRole      VARCHAR(16) NOT NULL DEFAULT '',
			job           VARCHAR(16) NOT NULL DEFAULT '',
			hullTypeID    BIGINT NOT NULL,
			hullClass     TINYINT NOT NULL DEFAULT 0,
			tank          VARCHAR(8) NOT NULL DEFAULT '',
			fitJSON       MEDIUMTEXT NULL,
			shipItemID    BIGINT NOT NULL DEFAULT 0,
			stationID     BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'planned',
			value         DOUBLE NOT NULL DEFAULT 0,
			plannedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (kitID),
			KEY idx_rbkit_corp (corporationID, kind, status),
			KEY idx_rbkit_ship (shipItemID)
		)`,
		// Where each kind of kit is kept (sticky; corparmory.go kitStation).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_armory (
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(8) NOT NULL,
			stationID     BIGINT NOT NULL,
			PRIMARY KEY (corporationID, kind)
		)`,
		// A kit in a member's hands (corparmory_issue.go). Deleted on return;
		// a terminal row (lost | stolen | written) is pruned after 30 days.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_issue (
			issueID       BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			human         TINYINT NOT NULL DEFAULT 0,
			kitID         BIGINT NOT NULL DEFAULT 0,
			kind          VARCHAR(8) NOT NULL,
			shipItemID    BIGINT NOT NULL,
			hullTypeID    BIGINT NOT NULL,
			value         DOUBLE NOT NULL DEFAULT 0,
			stationID     BIGINT NOT NULL,
			gangID        BIGINT NOT NULL DEFAULT 0,
			seatRole      VARCHAR(16) NOT NULL DEFAULT '',
			ownShipID     BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(10) NOT NULL DEFAULT 'issued',
			issuedAt      BIGINT NOT NULL DEFAULT 0,
			dueAt         BIGINT NOT NULL DEFAULT 0,
			warnedAt      BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (issueID),
			KEY idx_rbiss_char (characterID, status),
			KEY idx_rbiss_corp (corporationID, status),
			KEY idx_rbiss_ship (shipItemID)
		)`,
		// Who may never draw a kit again (or until untilAt): a thief, or a
		// pilot who brought one back stripped (corparmory_human.go).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_barred (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			reason        VARCHAR(8) NOT NULL,
			value         DOUBLE NOT NULL DEFAULT 0,
			issueID       BIGINT NOT NULL DEFAULT 0,
			barredAt      BIGINT NOT NULL DEFAULT 0,
			untilAt       BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// What a capsuleer took from a war-chest division, by type (corpsignout.go). qty is what is
		// still out; status is open, returned, lost or stolen.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_signout (
			signoutID     BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			stationID     BIGINT NOT NULL DEFAULT 0,
			division      INT NOT NULL DEFAULT 0,
			typeID        BIGINT NOT NULL,
			taken         BIGINT NOT NULL DEFAULT 0,
			qty           BIGINT NOT NULL DEFAULT 0,
			unitValue     DOUBLE NOT NULL DEFAULT 0,
			takenAt       BIGINT NOT NULL DEFAULT 0,
			warnedAt      BIGINT NOT NULL DEFAULT 0,
			lastKillID    BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			closedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (signoutID),
			KEY idx_signout_open (corporationID, status, characterID, typeID)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_corp_base_roles (
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			grantMask     BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID)
		)`,
		// A member's corporation job refused because every line of its kind at the base was queued out (corpbasepressure.go): the department's signal that the base is full.
		// kind is 'lab' or 'mfg'. Pruned after basePressureKeepSec.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_base_pressure (
			id            BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			stationID     BIGINT NOT NULL,
			kind          VARCHAR(4) NOT NULL,
			characterID   BIGINT NOT NULL DEFAULT 0,
			at            BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (id),
			KEY idx_rbcbp_corp (corporationID, kind, at)
		)`,
		// A print carried between a corporation's bases (corpprintmove.go): 'original' by a Director or the CEO, 'copy' by any member posted where it leaves.
		// status: open, claimed, carrying, done | void. libSource/libCost/libDonor ride along, since a print taken out of an office leaves the library.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_print_moves (
			moveID        BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			itemID        BIGINT NOT NULL,
			bpTypeID      BIGINT NOT NULL DEFAULT 0,
			kind          VARCHAR(8) NOT NULL DEFAULT 'original',
			fromStationID BIGINT NOT NULL,
			toStationID   BIGINT NOT NULL,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			status        VARCHAR(8) NOT NULL DEFAULT 'open',
			carrierID     BIGINT NOT NULL DEFAULT 0,
			human         TINYINT NOT NULL DEFAULT 0,
			jumps         INT NOT NULL DEFAULT 0,
			wage          DOUBLE NOT NULL DEFAULT 0,
			libSource     VARCHAR(12) NOT NULL DEFAULT '',
			libCost       DOUBLE NOT NULL DEFAULT 0,
			libDonor      BIGINT NOT NULL DEFAULT 0,
			postedAt      BIGINT NOT NULL DEFAULT 0,
			claimedAt     BIGINT NOT NULL DEFAULT 0,
			doneAt        BIGINT NOT NULL DEFAULT 0,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (moveID),
			KEY idx_rbcpm_corp (corporationID, status),
			KEY idx_rbcpm_item (itemID, status),
			KEY idx_rbcpm_carrier (carrierID, status)
		)`,
		// The corporate portal key (portalkey.go): the secret a pilot pastes
		// into the Management page to prove the terminal is theirs. Hash only.
		`CREATE TABLE IF NOT EXISTS rolebot_portal_keys (
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			keyHash       CHAR(64) NOT NULL,
			issuedAt      BIGINT NOT NULL DEFAULT 0,
			lastUsedAt    BIGINT NOT NULL DEFAULT 0,
			failCount     INT NOT NULL DEFAULT 0,
			failAt        BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID),
			KEY idx_rbpk_corp (corporationID)
		)`,
		// Orders from the chair (corpcommands.go): what a human CEO or Director posted on the Corporate Portal for the bots, with its whole life on the row.
		// Named 'command' because rolebot_corp_orders is the market-order ledger.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_commands (
			commandID     BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			posterID      BIGINT NOT NULL,
			posterHat     VARCHAR(16) NOT NULL DEFAULT '',
			kind          VARCHAR(16) NOT NULL,
			payload       TEXT NULL,
			note          VARCHAR(255) NOT NULL DEFAULT '',
			postedAt      BIGINT NOT NULL DEFAULT 0,
			expiresAt     BIGINT NOT NULL DEFAULT 0,
			status        VARCHAR(12) NOT NULL DEFAULT 'pending',
			claimedBy     BIGINT NOT NULL DEFAULT 0,
			claimedAt     BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			refKind       VARCHAR(16) NOT NULL DEFAULT '',
			refID         BIGINT NOT NULL DEFAULT 0,
			result        VARCHAR(255) NOT NULL DEFAULT '',
			attempts      INT NOT NULL DEFAULT 0,
			PRIMARY KEY (commandID),
			KEY idx_rbcmd_corp (corporationID, status),
			KEY idx_rbcmd_posted (corporationID, postedAt)
		)`,
		// What the corporation's finished goods actually cost it, per type: materials + fees + wages over units built. The company store prices from this.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_cost (
			corporationID BIGINT NOT NULL,
			typeID        BIGINT NOT NULL,
			units         BIGINT NOT NULL DEFAULT 0,
			cost          DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, typeID)
		)`,
		// The Ship Replacement Program (corpsrp.go): one row per member loss
		// the corporation has judged, keyed by the killmail.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_srp (
			killID        BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			shipTypeID    BIGINT NOT NULL,
			value         DOUBLE NOT NULL DEFAULT 0,
			status        VARCHAR(8) NOT NULL DEFAULT 'owed',
			itemID        BIGINT NOT NULL DEFAULT 0,
			lostAt        BIGINT NOT NULL DEFAULT 0,
			grantedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (killID),
			KEY idx_rbcsrp_corp (corporationID, status),
			KEY idx_rbcsrp_char (characterID, lostAt)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_corp_ranks (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			activity      VARCHAR(16) NOT NULL DEFAULT '',
			seat          INT NOT NULL DEFAULT 0,
			score         DOUBLE NOT NULL DEFAULT 0,
			manual        TINYINT NOT NULL DEFAULT 0,
			appointedAt   BIGINT NOT NULL DEFAULT 0,
			updatedAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID),
			KEY idx_rbrank_char (characterID),
			KEY idx_rbrank_corp (corporationID, activity)
		)`,
		// Goods a pilot holds as trade stock, with their cost. The hangar says what is there; this says which of it is inventory bought to be sold on.
		// Without it a trader could sell a miner's minerals or a fit's ammunition, and without the unit cost a sale books whatever it fetches as profit.
		`CREATE TABLE IF NOT EXISTS rolebot_trade_stock (
			characterID BIGINT NOT NULL,
			stationID   BIGINT NOT NULL,
			systemID    BIGINT NOT NULL DEFAULT 0,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			units       BIGINT NOT NULL DEFAULT 0,
			unitCost    DOUBLE NOT NULL DEFAULT 0,
			acquiredAt  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, stationID, typeID),
			KEY idx_rbstock_char (characterID)
		)`,
		// What a hunter has taken off dead rats and not sold yet (beltloot.go). The hold cannot answer that: it also holds the fit's ammunition.
		// The fence sells this ledger, min(recorded, actually present), so a whole-hold sale cannot sell reloads out from under the guns.
		`CREATE TABLE IF NOT EXISTS rolebot_loot_hold (
			characterID BIGINT NOT NULL,
			typeID      BIGINT NOT NULL,
			typeName    VARCHAR(100) NOT NULL DEFAULT '',
			units       BIGINT NOT NULL DEFAULT 0,
			takenAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, typeID),
			KEY idx_rbloot_char (characterID)
		)`,
		// Bot-to-bot commerce (tradebook.go): the pair, who took whose order at which station for how much, which the one-pilot trade book cannot say.
		// Written from the taker's side only, since the resting side would double every row; a bot's order taken by a player is not here.
		`CREATE TABLE IF NOT EXISTS rolebot_peer_trades (
			peerID    BIGINT NOT NULL AUTO_INCREMENT,
			ts        BIGINT NOT NULL,
			buyerID   BIGINT NOT NULL DEFAULT 0,
			sellerID  BIGINT NOT NULL DEFAULT 0,
			takerID   BIGINT NOT NULL DEFAULT 0,
			typeID    BIGINT NOT NULL DEFAULT 0,
			typeName  VARCHAR(100) NOT NULL DEFAULT '',
			quantity  BIGINT NOT NULL DEFAULT 0,
			price     DOUBLE NOT NULL DEFAULT 0,
			value     DOUBLE NOT NULL DEFAULT 0,
			stationID BIGINT NOT NULL DEFAULT 0,
			systemID  BIGINT NOT NULL DEFAULT 0,
			purpose   VARCHAR(32) NOT NULL DEFAULT '',
			PRIMARY KEY (peerID),
			KEY idx_rbpeer_ts (ts),
			KEY idx_rbpeer_buyer (buyerID, ts),
			KEY idx_rbpeer_seller (sellerID, ts),
			KEY idx_rbpeer_type (typeID),
			KEY idx_rbpeer_station (stationID)
		)`,
		// Lifetime counters (statcount.go): one row per rolebot, the module's only accumulator, in named columns because the Stats page is a pivot.
		// No secondary index: every read is the whole table and every write a primary-key upsert. Only the trade columns are backfilled (seedTradeCounters).
		`CREATE TABLE IF NOT EXISTS rolebot_stats (
			characterID  BIGINT NOT NULL,
			jumps        BIGINT NOT NULL DEFAULT 0,
			docks        BIGINT NOT NULL DEFAULT 0,
			oreUnits     BIGINT NOT NULL DEFAULT 0,
			oreM3        DOUBLE NOT NULL DEFAULT 0,
			trades       BIGINT NOT NULL DEFAULT 0,
			buys         BIGINT NOT NULL DEFAULT 0,
			sells        BIGINT NOT NULL DEFAULT 0,
			turnover     DOUBLE NOT NULL DEFAULT 0,
			profit       DOUBLE NOT NULL DEFAULT 0,
			bestTrade    DOUBLE NOT NULL DEFAULT 0,
			ordersPlaced BIGINT NOT NULL DEFAULT 0,
			feesPaid     DOUBLE NOT NULL DEFAULT 0,
			firstAt      BIGINT NOT NULL DEFAULT 0,
			lastAt       BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID)
		)`,
		// Where ore goes after the belt: a day bucket, not a log, one row per (day, type, stage), upserted and bounded, so one ranged DELETE is its only sweep.
		// isk is what the units were worth at the time (a sale's proceeds, a refine's input valued at the bid it passed up), never re-priced later.
		`CREATE TABLE IF NOT EXISTS rolebot_ore_flow (
			day    INT NOT NULL,
			typeID BIGINT NOT NULL,
			stage  VARCHAR(16) NOT NULL,
			units  BIGINT NOT NULL DEFAULT 0,
			m3     DOUBLE NOT NULL DEFAULT 0,
			isk    DOUBLE NOT NULL DEFAULT 0,
			PRIMARY KEY (day, typeID, stage)
		)`,
		// When a bot CEO's desk offered a human applicant an opening (corphuman.go): chrApplications has no 'offered at' column,
		// and an offer a player never answers would hold its opening forever.
		`CREATE TABLE IF NOT EXISTS rolebot_human_offers (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			offeredAt     BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// A member's standing in a bot-run corporation (corpmerit.go): merit points, the rank they bought and the role bits it granted.
		// rankRoleMask is the only set of bits the ladder removes, so officer and director grants stay. welcomedAt doubles as 'seen' (corphuman.go).
		`CREATE TABLE IF NOT EXISTS rolebot_corp_merit (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			points        DOUBLE NOT NULL DEFAULT 0,
			rankNo        INT NOT NULL DEFAULT 0,
			rankSince     BIGINT NOT NULL DEFAULT 0,
			lastActiveAt  BIGINT NOT NULL DEFAULT 0,
			welcomedAt    BIGINT NOT NULL DEFAULT 0,
			rankRoleMask  BIGINT UNSIGNED NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// How far the merit pass has read each corporation's ledger, and when it last paid out tenure, so points accrue once and are never recomputed.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_merit_cursor (
			corporationID BIGINT NOT NULL,
			ledgerEntryID BIGINT NOT NULL DEFAULT 0,
			tenureAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// How far the merit pass has read rolebot_corp_labour.
		`ALTER TABLE rolebot_corp_merit_cursor ADD COLUMN IF NOT EXISTS labourEntryID BIGINT NOT NULL DEFAULT 0`,
		// Why a member's points moved (corpmerit.go): the trail the promotion mail and the desk page quote. Pruned with the ledger.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_merit_log (
			logID         BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			at            BIGINT NOT NULL,
			points        DOUBLE NOT NULL DEFAULT 0,
			reason        VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (logID),
			KEY idx_rbcml_member (corporationID, characterID, at),
			KEY idx_rbcml_at (at)
		)`,
		// Corp bulletin lines waiting to be posted to the Corp mail tab (corpnews.go). Events append; one flush per newsMinSec posts them together,
		// so a busy CEO think is one bulletin.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_news (
			newsID        BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			kind          VARCHAR(16) NOT NULL DEFAULT '',
			at            BIGINT NOT NULL,
			line          VARCHAR(255) NOT NULL DEFAULT '',
			posted        TINYINT NOT NULL DEFAULT 0,
			aboutCharID   BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (newsID),
			KEY idx_rbcn_corp (corporationID, posted, at)
		)`,
		`CREATE TABLE IF NOT EXISTS rolebot_corp_news_state (
			corporationID BIGINT NOT NULL,
			lastPostAt    BIGINT NOT NULL DEFAULT 0,
			lastDigestAt  BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID)
		)`,
		// The Summit desk (summit.go). A story owns one forum thread.
		`CREATE TABLE IF NOT EXISTS rolebot_summit_stories (
			storyKey   VARCHAR(64) NOT NULL,
			threadID   BIGINT NOT NULL DEFAULT 0,
			category   VARCHAR(12) NOT NULL DEFAULT '',
			sideA      BIGINT NOT NULL DEFAULT 0,
			sideB      BIGINT NOT NULL DEFAULT 0,
			openedAt   BIGINT NOT NULL DEFAULT 0,
			lastPostAt BIGINT NOT NULL DEFAULT 0,
			posts      INT NOT NULL DEFAULT 0,
			state      VARCHAR(8) NOT NULL DEFAULT 'open',
			PRIMARY KEY (storyKey),
			KEY idx_rbss_thread (threadID)
		)`,
		// Posts waiting to be written. sourceKey is the dedupe: one incident,
		// one beat.
		`CREATE TABLE IF NOT EXISTS rolebot_summit_beats (
			beatID    BIGINT NOT NULL AUTO_INCREMENT,
			storyKey  VARCHAR(64) NOT NULL,
			category  VARCHAR(12) NOT NULL DEFAULT '',
			kind      VARCHAR(24) NOT NULL DEFAULT '',
			heat      DOUBLE NOT NULL DEFAULT 0,
			sourceKey VARCHAR(96) NOT NULL,
			facts     TEXT NOT NULL,
			sideA     BIGINT NOT NULL DEFAULT 0,
			sideB     BIGINT NOT NULL DEFAULT 0,
			speakCorp BIGINT NOT NULL DEFAULT 0,
			authorID  BIGINT NOT NULL DEFAULT 0,
			opens     TINYINT NOT NULL DEFAULT 0,
			notBefore BIGINT NOT NULL DEFAULT 0,
			expiresAt BIGINT NOT NULL DEFAULT 0,
			createdAt BIGINT NOT NULL DEFAULT 0,
			postedAt  BIGINT NOT NULL DEFAULT 0,
			dropped   TINYINT NOT NULL DEFAULT 0,
			dropWhy   VARCHAR(16) NOT NULL DEFAULT '',
			PRIMARY KEY (beatID),
			UNIQUE KEY uq_rbsb_source (sourceKey),
			KEY idx_rbsb_due (postedAt, dropped, notBefore),
			KEY idx_rbsb_story (storyKey)
		)`,
		// When each line was last used: the least-recently-used pick.
		`CREATE TABLE IF NOT EXISTS rolebot_summit_tpl (
			tplID    VARCHAR(64) NOT NULL,
			lastUsed BIGINT NOT NULL DEFAULT 0,
			uses     INT NOT NULL DEFAULT 0,
			PRIMARY KEY (tplID)
		)`,
		// Each bot that has posted: its fixed signature ("-" = none), and a
		// moderator's ban, which benches it.
		`CREATE TABLE IF NOT EXISTS rolebot_summit_authors (
			characterID BIGINT NOT NULL,
			signature   VARCHAR(255) NOT NULL DEFAULT '',
			lastPostAt  BIGINT NOT NULL DEFAULT 0,
			posts       INT NOT NULL DEFAULT 0,
			banned      TINYINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID)
		)`,
		// The desk's own post log: the budget's memory across a restart and
		// the panel's list. Pruned after 60 days.
		`CREATE TABLE IF NOT EXISTS rolebot_summit_posts (
			postLogID   BIGINT NOT NULL AUTO_INCREMENT,
			threadID    BIGINT NOT NULL DEFAULT 0,
			characterID BIGINT NOT NULL DEFAULT 0,
			storyKey    VARCHAR(64) NOT NULL DEFAULT '',
			kind        VARCHAR(24) NOT NULL DEFAULT '',
			category    VARCHAR(12) NOT NULL DEFAULT '',
			newThread   TINYINT NOT NULL DEFAULT 0,
			at          BIGINT NOT NULL DEFAULT 0,
			excerpt     VARCHAR(255) NOT NULL DEFAULT '',
			PRIMARY KEY (postLogID),
			KEY idx_rbsp_at (at)
		)`,
		// A paid assignment offered to one human member (corpassign.go). Completion is read off what the member does
		// (a deposit into the corp office, a delivered corporation job, NPC kills in a system), never off a click.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_assignments (
			assignmentID  BIGINT NOT NULL AUTO_INCREMENT,
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			kind          VARCHAR(12) NOT NULL,
			typeID        BIGINT NOT NULL DEFAULT 0,
			qty           BIGINT NOT NULL DEFAULT 0,
			progress      BIGINT NOT NULL DEFAULT 0,
			stationID     BIGINT NOT NULL DEFAULT 0,
			division      INT NOT NULL DEFAULT 0,
			systemID      BIGINT NOT NULL DEFAULT 0,
			workID        BIGINT NOT NULL DEFAULT 0,
			rewardISK     DOUBLE NOT NULL DEFAULT 0,
			paidISK       DOUBLE NOT NULL DEFAULT 0,
			meritPts      DOUBLE NOT NULL DEFAULT 0,
			status        VARCHAR(10) NOT NULL DEFAULT 'offered',
			offeredBy     BIGINT NOT NULL DEFAULT 0,
			offeredAt     BIGINT NOT NULL DEFAULT 0,
			acceptedAt    BIGINT NOT NULL DEFAULT 0,
			expiresAt     BIGINT NOT NULL DEFAULT 0,
			closedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (assignmentID),
			KEY idx_rbca_member (characterID, status),
			KEY idx_rbca_corp (corporationID, status)
		)`,
		// The job board's letters to one member (corpassign.go): when the last 'new work' notice went out and how many went unanswered in a row;
		// after three ignored the board stops writing until a take.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_assign_notice (
			corporationID BIGINT NOT NULL,
			characterID   BIGINT NOT NULL,
			lastNoticeAt  BIGINT NOT NULL DEFAULT 0,
			unanswered    INT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, characterID)
		)`,
		// Safe spots (safespot.go): a point in warp deadspace a pilot bookmarked mid-warp, owned by the corporation that made it (fleet mates hold copies).
		// purpose is 'stage', 'escape' or 'logoff'; usedAt and burnedAt are unix seconds, 0 = never; a burned row is never picked again.
		`CREATE TABLE IF NOT EXISTS rolebot_safespots (
			safeID        BIGINT NOT NULL AUTO_INCREMENT,
			corpID        BIGINT NOT NULL DEFAULT 0,
			solarSystemID BIGINT NOT NULL DEFAULT 0,
			x             DOUBLE NOT NULL DEFAULT 0,
			y             DOUBLE NOT NULL DEFAULT 0,
			z             DOUBLE NOT NULL DEFAULT 0,
			bookmarkID    BIGINT NOT NULL DEFAULT 0,
			nearStationID BIGINT NOT NULL DEFAULT 0,
			purpose       VARCHAR(8) NOT NULL DEFAULT 'stage',
			createdBy     BIGINT NOT NULL DEFAULT 0,
			createdAt     BIGINT NOT NULL DEFAULT 0,
			usedAt        BIGINT NOT NULL DEFAULT 0,
			burnedAt      BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (safeID),
			KEY idx_rbsafe_pick (corpID, solarSystemID, purpose, burnedAt),
			KEY idx_rbsafe_live (burnedAt, solarSystemID)
		)`,
	}
	// Drop the old per-character rolebot_fits shape, which holds no data worth keeping.
	var legacy int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_fits'
		  AND COLUMN_NAME = 'characterID'`).Scan(&legacy); err == nil && legacy > 0 {
		if _, err := db.ExecContext(ctx, `DROP TABLE rolebot_fits`); err != nil {
			return fmt.Errorf("ensureTables drop legacy rolebot_fits: %w", err)
		}
	}

	for _, q := range stmts {
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensureTables: %w", err)
		}
	}

	// Profile and other column additions (MariaDB supports IF NOT EXISTS on ADD COLUMN).
	alters := []string{
		// homeStationID is gone from rolebot_profiles: home is the clone station (character_.homeStationID), the industry base the only other address.
		// migrateProfileHome moves a pinned value onto the clone first. oreDisposal is dropped too: miningConfigDerived (activitycfg.go) derives it from the trip.
		`ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS oreDisposal`,
		`ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS stockpileStationID`,
		// The per-pilot security fence (secLimitOn + secFloor) is dropped: security is a price, not a wall (rulesForBot),
		// and only an activity's own config sets a floor, for that job.
		`ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS secLimitOn`,
		`ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS secFloor`,
		// The preferred-region list is dropped: a pilot's space comes from where it lives now (homeRegionSet; employer corphome.go, operator pin),
		// so a stored list would look authoritative and mislead. The operator's blacklist is untouched: a hard fence set by hand.
		`ALTER TABLE rolebot_profiles DROP COLUMN IF EXISTS regionsPreferred`,
		// Failsafe bookkeeping: consecutive failures, the last one, and the halt flag the panel's Reboot button clears.
		// rolebot_generated is keyed on (seq, slot) because pilots share an account; the old UNIQUE on seq alone is dropped.
		`ALTER TABLE rolebot_generated ADD COLUMN IF NOT EXISTS slot INT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_generated DROP INDEX IF EXISTS uq_rbgen_seq`,
		`ALTER TABLE rolebot_generated ADD UNIQUE KEY IF NOT EXISTS uq_rbgen_slot (seq, slot)`,
		`ALTER TABLE rolebot_generated ADD KEY IF NOT EXISTS idx_rbgen_seq (seq)`,
		// The open book. rolebot_orders keeps every order ever placed, so open ones are a thin slice, and the roster-wide reads of that slice
		// (mineralWants, the escrow summaries, tradebook's reconcile) would scan it all. This key serves them.
		`ALTER TABLE rolebot_orders ADD KEY IF NOT EXISTS idx_rborder_open (closedAt, bid, kind)`,
		// bidCandidateTypes asks the trade book what sold at one station in the last month on every bid roll. Kind second and the read columns last make it
		// two index-only ranges, not a clustered-row fetch per leg. The old key is dropped only after this one exists, so the query never has neither.
		`ALTER TABLE rolebot_trades ADD KEY IF NOT EXISTS idx_rbtrade_station_kind (stationID, kind, ts, typeID, characterID)`,
		`ALTER TABLE rolebot_trades DROP INDEX IF EXISTS idx_rbtrade_station`,
		// The dead-shelf table and the spec calibration read one kind of row (pulls, fills) over a window;
		// without a kind key both scan the whole book.
		`ALTER TABLE rolebot_trades ADD KEY IF NOT EXISTS idx_rbtrade_kind_ts (kind, ts)`,
		// The system drill-down under Economy, Hubs (esphubsystem.go) reads one system's recent legs and peer trades over a window;
		// without a system key either scans the whole book.
		`ALTER TABLE rolebot_trades ADD KEY IF NOT EXISTS idx_rbtrade_sys_ts (systemID, ts)`,
		`ALTER TABLE rolebot_peer_trades ADD KEY IF NOT EXISTS idx_rbpeer_sys_ts (systemID, ts)`,
		// A leg that bought goods to sell on (a hauler loading a run, a bid filling into trade stock, ore bought to flip). The trade tape (tradeenduse.go) takes
		// these off a type's sales, since a trader buying is not a customer. Keyed by type first, because the tape reads one type over a fortnight.
		`ALTER TABLE rolebot_trades ADD COLUMN IF NOT EXISTS resale TINYINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_trades ADD KEY IF NOT EXISTS idx_rbtrade_type_resale (typeID, resale, ts)`,
		// The market history's recent read: every type's resale legs of a few
		// days, which the per-type key above cannot serve.
		`ALTER TABLE rolebot_trades ADD KEY IF NOT EXISTS idx_rbtrade_resale_ts (resale, ts)`,
		// The security band a capsuleer was generated into became a lawful/criminal disposition (genplan.go). `band` is kept:
		// it is the only record of what an older row was generated as, and the population list falls back to it when lawfulness is empty.
		`ALTER TABLE rolebot_generated ADD COLUMN IF NOT EXISTS lawfulness VARCHAR(12) NOT NULL DEFAULT ''`,
		// A can jettisoned under a player's orders belongs to that fleet, which has no rolebot_gangs row, so the can registry has an owner of the other kind (bossCharID).
		// gangID stays 0 for those, which keeps sweepCans and the operator-fleet hauler from adopting a player's ore.
		`ALTER TABLE rolebot_gang_cans ADD COLUMN IF NOT EXISTS bossCharID BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_gang_cans ADD KEY IF NOT EXISTS idx_rbcan_boss (bossCharID)`,
		// How many containers this one continues (cans.go rolloverCan): a can nearing the core's 2 h expiry with ore still in it is rolled
		// into a fresh one rather than deleted with its contents.
		`ALTER TABLE rolebot_gang_cans ADD COLUMN IF NOT EXISTS rollovers INT NOT NULL DEFAULT 0`,
		// When the ore in a rolled can was first canned (cans.go). createdAt restarts at every rollover (it is the core container's clock);
		// the hauler queues on this one, so the oldest rock keeps its place. 0 on older rows reads as createdAt.
		`ALTER TABLE rolebot_gang_cans ADD COLUMN IF NOT EXISTS firstCreatedAt BIGINT NOT NULL DEFAULT 0`,
		// The day a player corporation last sent a fleet live, per activity (miningop.go). Mining ops are rationed:
		// stamped when the op goes live, not when it forms, so a muster that never sails does not spend the day.
		`CREATE TABLE IF NOT EXISTS rolebot_corp_fleet_day (
			corporationID BIGINT NOT NULL,
			activity      VARCHAR(16) NOT NULL,
			lastLiveAt    BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (corporationID, activity)
		)`,
		// The go-live before lastLiveAt, for a corporation allowed two ops a
		// day (miningop.go miningOpRationFor).
		`ALTER TABLE rolebot_corp_fleet_day ADD COLUMN IF NOT EXISTS prevLiveAt BIGINT NOT NULL DEFAULT 0`,
		// The factory the estate power chose (corpfacility.go): it outranks the headquarters in corpFactoryStation,
		// so a desk moved to a station with research lines stays moved. 0 = no pin.
		`ALTER TABLE rolebot_corp_policy ADD COLUMN IF NOT EXISTS factoryStationID BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_state ADD COLUMN IF NOT EXISTS errCount INT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_state ADD COLUMN IF NOT EXISTS lastError TEXT NULL`,
		`ALTER TABLE rolebot_state ADD COLUMN IF NOT EXISTS lastErrorAt BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_state ADD COLUMN IF NOT EXISTS halted TINYINT NOT NULL DEFAULT 0`,
		// Industry claims on the demand ledger: one supplier per type at a time (claimedBy/claimedAt, expiry enforced in the claim UPDATE),
		// servedAt stamped when a supplier's sale settles the row.
		`ALTER TABLE rolebot_market_demand ADD COLUMN IF NOT EXISTS claimedBy BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_market_demand ADD COLUMN IF NOT EXISTS claimedAt BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_market_demand ADD COLUMN IF NOT EXISTS servedAt BIGINT NOT NULL DEFAULT 0`,
		// The census counts two populations (wants.go): everything a pilot could fit or fire, even with no ordinary blueprint (meta, faction gear),
		// and buildable = 1, what an industrialist can act on, which the build book and the wants page read.
		`ALTER TABLE rolebot_capability ADD COLUMN IF NOT EXISTS buildable TINYINT NOT NULL DEFAULT 1`,
		// The production headcount (wantsusers.go): pilots who fit, load or fly the type, beside the skill headcount in capable. Added at -1 and backfilled to capable,
		// since a 0 would empty the capability tier and veto every owned print until the next census; the census never writes a negative, so the backfill is a no-op later.
		`ALTER TABLE rolebot_capability ADD COLUMN IF NOT EXISTS users BIGINT NOT NULL DEFAULT -1`,
		`UPDATE rolebot_capability SET users = capable WHERE users < 0`,
		// The roster's own unsold stock of the type (wantsusers.go rateHeld): packaged units in its pilots' hangars and its corporations' offices,
		// which the flood test counts beside the listings. 0 until the next census.
		`ALTER TABLE rolebot_capability ADD COLUMN IF NOT EXISTS held BIGINT NOT NULL DEFAULT 0`,
		// Operator's per-pilot factory pin; 0 = the pilot chooses its own.
		`ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS industryStationID BIGINT NOT NULL DEFAULT 0`,
		// Operator pin on this pilot's address (clone and factory): nothing may move either until released (corphome.go, moveout.go, clone.go, industry.go).
		// One flag, not a station: home is character_.homeStationID only, and a stored address would be a second copy that disagrees.
		`ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS homePinned TINYINT NOT NULL DEFAULT 0`,
		// Whether this pilot invests in its own skills (skills.go). Defaults to 1: a pilot that does not train stays frozen at the sheet it was adopted with,
		// and existing pilots start spending their own ISK on skillbooks.
		`ALTER TABLE rolebot_profiles ADD COLUMN IF NOT EXISTS autoSkills TINYINT NOT NULL DEFAULT 1`,
		// Which kind of goal pulled a plan step in (skills.go goal* set), the planner's own vocabulary beside the why text; the Skills page groups on it.
		// Rows from an older build read as empty and age out within one replan interval.
		`ALTER TABLE rolebot_skill_plan ADD COLUMN IF NOT EXISTS kind VARCHAR(16) NOT NULL DEFAULT ''`,
		// The hub score also measures the map's shape (gates), security, the player/bot versus NPC-seeder book split and the parasite pass (traffic.go).
		// belt_distress gets corpID by ADD COLUMN here; re-keying is guarded in migrateBeltDistressCorp, since a bare DROP PRIMARY KEY on a keyless table throws and kills init.
		`ALTER TABLE rolebot_belt_distress ADD COLUMN IF NOT EXISTS corpID BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_belt_distress ADD KEY IF NOT EXISTS idx_rbdistress_corp (corpID, lastAt)`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS stationTerm DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS topoTerm DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS secTerm DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS secUnit DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS base DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS shadow DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS shadowBy BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS gates INT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS playerOrders BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS playerISK DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS school TINYINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS schoolCut DOUBLE NOT NULL DEFAULT 0`,
		// The epoch dice (traffic.go applyHubNoise): the signed offset this epoch's seed dealt the system, already folded into base,
		// stored so the map page can show what luck gave each system.
		`ALTER TABLE rolebot_hub_scores ADD COLUMN IF NOT EXISTS noise DOUBLE NOT NULL DEFAULT 0`,
		// The dock contest's anchors term and the hub score's anchorTerm, secUnit, shadow, shadowBy, schoolCut and noise are inert (written zero, columns kept);
		// measured liquidity stands in for anchors (stationscore.go, traffic.go).
		`ALTER TABLE rolebot_station_scores ADD COLUMN IF NOT EXISTS liquidityTerm DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_station_scores ADD COLUMN IF NOT EXISTS liquidity DOUBLE NOT NULL DEFAULT 0`,
		// This world's luck for the dock and the real players' share of its book (fortune.go).
		`ALTER TABLE rolebot_station_scores ADD COLUMN IF NOT EXISTS fortuneTerm DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_station_scores ADD COLUMN IF NOT EXISTS humanOrders BIGINT NOT NULL DEFAULT 0`,
		// seeded says the row is still exactly what defaultCorpPolicy made it; saveCorpPolicy clears it, so a change of defaults reaches only rows nobody edited.
		// DEFAULT 1 is right for an existing table (the seeder wrote every row); applyCorpDefaultGeneration corrects hand-opened ones once.
		`ALTER TABLE rolebot_corps ADD COLUMN IF NOT EXISTS seeded TINYINT NOT NULL DEFAULT 1`,
		// The faction a player corporation flies for (allegiance.go); 0 is unaligned. Written only by setCorpAllegiance.
		`ALTER TABLE rolebot_corps ADD COLUMN IF NOT EXISTS allegiance BIGINT NOT NULL DEFAULT 0`,
		// Where a displaced pilot goes home to, and where the module last put it (corpjoin.go). Together they are how a kick is noticed.
		`ALTER TABLE rolebot_corp_reviews ADD COLUMN IF NOT EXISTS schoolCorpID BIGINT NOT NULL DEFAULT 0`,
		`ALTER TABLE rolebot_corp_reviews ADD COLUMN IF NOT EXISTS heldCorpID BIGINT NOT NULL DEFAULT 0`,
		// When the morale sampler decided this pilot leaves (corpleave.go); 0 none.
		`ALTER TABLE rolebot_corp_reviews ADD COLUMN IF NOT EXISTS exitDueAt BIGINT NOT NULL DEFAULT 0`,
		// The operator's dividend override (corpdividend.go): payoutPct < 0
		// and dividendDays 0 mean "the charter decides".
		`ALTER TABLE rolebot_share_state ADD COLUMN IF NOT EXISTS payoutPct DOUBLE NOT NULL DEFAULT -1`,
		`ALTER TABLE rolebot_share_state ADD COLUMN IF NOT EXISTS dividendDays DOUBLE NOT NULL DEFAULT 0`,
		// Who did the work a CEO ledger line records (corpstaff.go): 0 for the
		// CEO itself, else the officer.
		`ALTER TABLE rolebot_ceo_actions ADD COLUMN IF NOT EXISTS actorID BIGINT NOT NULL DEFAULT 0`,
		// A member's trade-levy profit not charged yet (corptradelevy.go): charged once a day or past 100k ISK of levy.
		`CREATE TABLE IF NOT EXISTS rolebot_levy_due (
			characterID   BIGINT NOT NULL,
			corporationID BIGINT NOT NULL,
			profit        DOUBLE NOT NULL DEFAULT 0,
			since         BIGINT NOT NULL DEFAULT 0,
			PRIMARY KEY (characterID, corporationID)
		)`,
		// The founder's sector at founding (corpfound_sector.go), for a new corporation the identity power has not tagged yet.
		`ALTER TABLE rolebot_founded_corps ADD COLUMN IF NOT EXISTS sector VARCHAR(16) NOT NULL DEFAULT ''`,
		// A library purchase order that may be filled with a blueprint copy (corpjobs.go corpPOArm): a shipyard's hull orders,
		// so a corporation that cannot afford a hull original can build from a copy.
		`ALTER TABLE rolebot_corp_po ADD COLUMN IF NOT EXISTS allowCopy TINYINT NOT NULL DEFAULT 0`,
		// A kit bought for one pilot to bring to a siege (siegekits.go): it is lent to that pilot first,
		// and the shelf's own targets do not count it.
		`ALTER TABLE rolebot_corp_kit ADD COLUMN IF NOT EXISTS forCharID BIGINT NOT NULL DEFAULT 0`,
		// Drop the retired per-belt m3 budget table (bots mine the seeded field, beltfield.go).
		`DROP TABLE IF EXISTS rolebot_belt_state`,
	}
	// Skip the ones already in force: every ALTER, even one whose IF [NOT] EXISTS clause
	// makes it a no-op, takes an exclusive metadata lock and can touch the table
	// definition, and ~70 of them ran on every boot. One information_schema read
	// answers them all; anything the snapshot cannot judge still runs.
	snap := loadSchemaSnapshot(ctx, db)
	for _, q := range alters {
		if snap.alterApplied(q) {
			continue
		}
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("ensureTables alter: %w", err)
		}
	}
	ensureLogIndexes(ctx, db)
	ensureMarketIndexes(ctx, db)
	ensureCorpIndexes(ctx, db)
	if err := migrateGangOrePerLoad(ctx, db); err != nil {
		return err
	}

	// Mineral VWAP (mineralDigPrices) reads seven typeIDs over three days; the core's date-first idx_mt_type_date_agg walks the whole window,
	// so type before date (regionID last) makes seven short covering ranges. A game table: probed first (haveTable); a failure is logged, not fatal.
	if ok, err := haveTable(ctx, db, "market_transactions"); err == nil && ok {
		if _, err := db.ExecContext(ctx, `ALTER TABLE market_transactions
			ADD KEY IF NOT EXISTS idx_mt_type_item_date
			(transactionType, typeID, transactionDateTime, quantity, price, regionID)`); err != nil {
			rlogf("rolebots: market_transactions VWAP index: %v", err)
		}
	}

	if err := migrateProfileHome(ctx, db); err != nil {
		return err
	}

	if err := migrateBeltDistressCorp(ctx, db); err != nil {
		return err
	}

	if err := migrateWorkClaimsPerJob(ctx, db); err != nil {
		return err
	}

	// Re-key rolebot_fit_ships off the ship itself (a generated fit's ID is the negative of its hull type), so a pilot's hull is not mistaken for missing and bought twice.
	// See docs/comment-notes-b18.md#rekey-fit-ships.
	haveEntity, err := haveTable(ctx, db, "entity")
	if err != nil {
		return err
	}
	if haveEntity {
		// A test stand-in may carry an entity table without typeID; the
		// statements below cannot run against it.
		if haveEntity, err = haveColumn(ctx, db, "entity", "typeID"); err != nil {
			return err
		}
	}
	rekey := []string{}
	if haveEntity {
		rekey = []string{
			`DELETE s FROM rolebot_fit_ships s
		 JOIN entity e ON e.itemID = s.shipItemID
		 JOIN rolebot_fit_ships k ON k.characterID = s.characterID AND k.fitID = -e.typeID
		 WHERE s.fitID > 0`,
			`DELETE s FROM rolebot_fit_ships s
		 JOIN entity e ON e.itemID = s.shipItemID
		 JOIN rolebot_fit_ships k ON k.characterID = s.characterID AND k.fitID > 0 AND k.fitID < s.fitID
		 JOIN entity ke ON ke.itemID = k.shipItemID AND ke.typeID = e.typeID
		 WHERE s.fitID > 0`,
			`UPDATE rolebot_fit_ships s
		 JOIN entity e ON e.itemID = s.shipItemID
		 SET s.fitID = -e.typeID
		 WHERE s.fitID > 0`,
			`DELETE FROM rolebot_fit_ships WHERE fitID > 0`,
		}
	}
	for _, stmt := range rekey {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("ensureTables re-key rolebot_fit_ships: %w", err)
		}
	}

	// Delete one-magazine ammunition shelf lines: a line's target was the batch just built, so a supplier held one magazine forever. An operator's pin stays.
	// Probed on the columns, not just the tables: the harness stand-ins for invTypes and invGroups lack portionSize.
	shelfReady, err := haveAllColumns(ctx, db,
		"invTypes.typeID", "invTypes.groupID", "invTypes.portionSize",
		"invGroups.groupID", "invGroups.categoryID")
	if err != nil {
		return err
	}
	if shelfReady {
		if _, err := db.ExecContext(ctx, `
			DELETE s FROM rolebot_industry_shelf s
			JOIN invTypes t ON t.typeID = s.typeID
			JOIN invGroups g ON g.groupID = t.groupID
			WHERE s.source <> 'operator' AND g.categoryID = 8
			  AND s.targetQty <= t.portionSize`); err != nil {
			return fmt.Errorf("ensureTables clear one-magazine shelf lines: %w", err)
		}
	}

	var assignTable int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_fit_assignments'`,
	).Scan(&assignTable); err == nil && assignTable > 0 {
		// Only rows with a real hull carry over; an assignment with no ship was permission, not ownership.
		if _, err := db.ExecContext(ctx, `
			INSERT IGNORE INTO rolebot_fit_ships (characterID, fitID, shipItemID)
			SELECT characterID, fitID, shipItemID
			FROM rolebot_fit_assignments WHERE shipItemID <> 0`); err != nil {
			return fmt.Errorf("ensureTables carry ships over: %w", err)
		}
		if _, err := db.ExecContext(ctx, `DROP TABLE rolebot_fit_assignments`); err != nil {
			return fmt.Errorf("ensureTables drop rolebot_fit_assignments: %w", err)
		}
	}

	// A pilot stores no standings now, toward a faction (its employer's politics, outlook.go) or toward another pilot (standings.go).
	// This takes both tables, the in-game contact rows that mirrored the pilot half, and the legacy CSV columns.
	if err := migrateDropPilotStandings(ctx, db); err != nil {
		return err
	}
	// Caution, patience and loyalty use the hostile-end-is-+100 polarity everywhere; stored rows flip to match the wiring.
	if err := migrateTraitPolarity(ctx, db); err != nil {
		return err
	}
	// The leadership axis arrived after the roster did; every pilot without a
	// value draws one, once (leadership.go).
	if err := migrateLeadershipSeed(ctx, db); err != nil {
		return err
	}
	// And the tolerance axis after it, the same way (tolerance.go).
	if err := migrateToleranceSeed(ctx, db); err != nil {
		return err
	}
	// Toil, the trait that owns mining (toil.go): the roster's draw, a fifth of the traders turned haulers, then the pass over bot CEOs
	// whose corporations' business is plain, in that order because each reads what the one before wrote.
	if err := migrateToilSeed(ctx, db); err != nil {
		return err
	}
	if err := migrateHaulerConversion(ctx, db); err != nil {
		return err
	}
	if err := migrateIndustrialistToilShift(ctx, db); err != nil {
		return err
	}
	if err := migrateCEOIdentity(ctx, db); err != nil {
		return err
	}
	if err := migrateCEOResearchIdentity(ctx, db); err != nil {
		return err
	}
	if err := migrateResearchSeed(ctx, db); err != nil {
		return err
	}
	// The hours each pilot plays, and a CEO's corporation mostly on the
	// CEO's (timezone.go). After the CEO passes, which seat the CEOs.
	if err := migrateTimezoneSeed(ctx, db); err != nil {
		return err
	}
	if err := migrateClearHomePins(ctx, db); err != nil {
		return err
	}
	if err := migrateGraceSeed(ctx, db); err != nil {
		return err
	}
	// The activity checklist is gone: what a bot does is decided by its appetites over its flags (appetite.go), not an enabled-bit per activity.
	// The mining disposal is derived from the trip (activitycfg.go), so the table just goes.
	if err := migrateActivityChecklist(ctx, db); err != nil {
		return err
	}
	// Gang leader and formation-sweep bookkeeping columns (gangform.go); older tables get them here.
	if err := migrateGangLeaderColumns(ctx, db); err != nil {
		return err
	}
	// The corporation's arms (corparm.go), before the fleet-column migration that adds rolebot_corp_arm_members.setBy:
	// on a fresh schema that ALTER would otherwise hit a missing table.
	if err := ensureArmTables(ctx, db); err != nil {
		return err
	}
	// Who a human CEO put in charge of each division (corpheads.go), and the pay its wallet owes (corpowed.go).
	if err := ensureHeadTables(ctx, db); err != nil {
		return err
	}
	if err := ensureOwedTable(ctx, db); err != nil {
		return err
	}
	// Finance's weekly envelopes per arm (corpenvelope.go).
	if err := ensureEnvelopeTables(ctx, db); err != nil {
		return err
	}
	// Fleets are corporation functions (corprank.go): gangs remember whose corp sent them out,
	// and a player corp can point its mining fleets at a corp-hangar station.
	if err := migrateCorpFleetColumns(ctx, db); err != nil {
		return err
	}
	// Bot CEOs (corpceo.go): a player corporation's operator may ask for
	// titles to be kept in step with its seats without handing it to a CEO.
	if err := migrateCorpTitleColumn(ctx, db); err != nil {
		return err
	}
	// Bot CEOs are banned from moving their headquarters until the operator
	// lifts it on the CEO page (corpceo_estate.go).
	if err := migrateCEOHQMovableColumn(ctx, db); err != nil {
		return err
	}
	// ...except the corporations bots founded themselves, once (corpceo.go).
	if err := grantFoundedHQMovable(ctx, db); err != nil {
		return err
	}
	// A bulletin line names the capsuleer it is about (corpnews.go).
	if err := migrateCorpNewsAboutColumn(ctx, db); err != nil {
		return err
	}
	// Fleets can second a combat pilot from another corporation (gangescort.go), so a member row records which corp it came from.
	if err := migrateGangEscortColumn(ctx, db); err != nil {
		return err
	}
	// Rename stored belthunt slugs to bounty (combatplan.go), so no hunter wakes with an activity no planner owns.
	if err := migrateBountyRename(ctx, db); err != nil {
		return err
	}
	// Then bounty becomes combat (combatplan.go): the same four homes.
	if err := migrateCombatRename(ctx, db); err != nil {
		return err
	}
	// The looter seat is retired (gangs.go); its rows become gun seats.
	if err := migrateLooterSeat(ctx, db); err != nil {
		return err
	}
	// The fleet loot ledger is retired: every pilot loots for itself, and what a carrier held for a fleet becomes its own.
	if err := migrateGangLootLedger(ctx, db); err != nil {
		return err
	}
	// Corporation fleet doctrines (fleetdoctrine_db.go).
	if err := ensureDoctrineTables(ctx, db); err != nil {
		return err
	}
	// Bot corporations' starbases (postables.go).
	if err := ensureStarbaseTables(ctx, db); err != nil {
		return err
	}
	// Who holds which seat (corpposts.go): carries and drops the three predecessor tables once they are no longer created.
	if err := ensurePostTables(ctx, db); err != nil {
		return err
	}
	// The grace ledger (grace_ledger.go).
	if err := ensureGraceTables(ctx, db); err != nil {
		return err
	}
	// The relations ledger (corprelations.go): carries and drops rolebot_ceo_relations once it is no longer created.
	if err := ensureRelationTables(ctx, db); err != nil {
		return err
	}
	// The skill lane's per-pilot plan summary (skills.go): rebuilt from the plan rows every boot.
	if err := rebuildSkillDue(ctx, db); err != nil {
		return err
	}
	return nil
}

// migrateBountyRename rewrites every persisted "belthunt" slug to "bounty" in four homes: the state row's activity and phase, the gang row's activity,
// the corp rank's activity, and the quoted slug in the StateData JSON blob. The blob REPLACE is safe because the quoted token only appears as a slug.
// Idempotent: every statement matches nothing on a second run.
func migrateBountyRename(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`UPDATE rolebot_state SET activity = 'bounty' WHERE activity = 'belthunt'`,
		`UPDATE rolebot_state SET phase = CONCAT('bounty', SUBSTRING(phase, 9))
		 WHERE phase LIKE 'belthunt.%'`,
		`UPDATE rolebot_gangs SET activity = 'bounty' WHERE activity = 'belthunt'`,
		`UPDATE rolebot_corp_ranks SET activity = 'bounty' WHERE activity = 'belthunt'`,
		`UPDATE rolebot_state SET data = REPLACE(data, '"belthunt"', '"bounty"')
		 WHERE data LIKE '%"belthunt"%'`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrateBountyRename: %w", err)
		}
	}
	return nil
}

// migrateCombatRename rewrites every persisted "bounty" activity slug to "combat", and the "bounty.*" phase prefix with it, in the same four homes as migrateBountyRename.
// SUBSTRING(phase, 7) keeps the "." and the step. Idempotent. It must run after migrateBountyRename, or a roster still carrying 'belthunt' would skip past it.
func migrateCombatRename(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`UPDATE rolebot_state SET activity = 'combat' WHERE activity = 'bounty'`,
		`UPDATE rolebot_state SET phase = CONCAT('combat', SUBSTRING(phase, 7))
		 WHERE phase LIKE 'bounty.%'`,
		`UPDATE rolebot_gangs SET activity = 'combat' WHERE activity = 'bounty'`,
		`UPDATE rolebot_corp_ranks SET activity = 'combat' WHERE activity = 'bounty'`,
		`UPDATE rolebot_state SET data = REPLACE(data, '"bounty"', '"combat"')
		 WHERE data LIKE '%"bounty"%'`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrateCombatRename: %w", err)
		}
	}
	return nil
}

// migrateLooterSeat retires the looter seat: every "looter" membership row becomes a gun seat (RoleCombat), and pilots holding the old "gang.loot" phase,
// the "seat.loot" activity or a "nextPhase":"gang.loot" key are reset to idle. Idempotent. See docs/comment-notes-b18.md#migrate-looter-seat.
func migrateLooterSeat(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`UPDATE rolebot_gang_members SET role = 'combat' WHERE role = 'looter'`,
		`UPDATE rolebot_state SET activity = '', phase = 'idle'
		 WHERE phase = 'gang.loot' OR activity = 'seat.loot'`,
		`UPDATE rolebot_state SET data = REPLACE(data, '"seat.loot"', '"combat"')
		 WHERE data LIKE '%"seat.loot"%'`,
		`UPDATE rolebot_state SET data = REPLACE(data, '"nextPhase":"gang.loot"', '"nextPhase":"idle"')
		 WHERE data LIKE '%"nextPhase":"gang.loot"%'`,
	}
	for _, stmt := range stmts {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrateLooterSeat: %w", err)
		}
	}
	return nil
}

// migrateGangLootLedger retires rolebot_gang_loot. Rows still standing are folded into each carrier's own ledger (rolebot_loot_hold), since it
// physically holds the items, and the table is dropped; every pilot now loots for itself (lootdonate.go). Idempotent.
func migrateGangLootLedger(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_gang_loot'`).Scan(&n); err != nil {
		return fmt.Errorf("migrateGangLootLedger: %w", err)
	}
	if n == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_loot_hold (characterID, typeID, typeName, units, takenAt)
		SELECT characterID, typeID, MAX(typeName), SUM(units), MAX(takenAt)
		  FROM rolebot_gang_loot WHERE units > 0
		 GROUP BY characterID, typeID
		ON DUPLICATE KEY UPDATE units = units + VALUES(units), takenAt = VALUES(takenAt)`); err != nil {
		return fmt.Errorf("migrateGangLootLedger fold: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE rolebot_gang_loot`); err != nil {
		return fmt.Errorf("migrateGangLootLedger drop: %w", err)
	}
	return nil
}

// migrateCorpFleetColumns adds rolebot_gangs.corpID (which corporation's fleet this is) and rolebot_corps.fleetOreStationID (a player corp's mining
// drop-off; 0 = sell as usual) to older tables. Probed per table, since an operator may have restored one without the other.
func migrateCorpFleetColumns(ctx context.Context, db *sql.DB) error {
	addColumn := func(table, column, ddl string) error {
		var have int
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.COLUMNS
			WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
			  AND COLUMN_NAME = ?`, table, column).Scan(&have); err != nil {
			return fmt.Errorf("migrateCorpFleetColumns probe %s.%s: %w", table, column, err)
		}
		if have > 0 {
			return nil
		}
		if _, err := db.ExecContext(ctx,
			"ALTER TABLE "+table+" ADD COLUMN "+column+" "+ddl); err != nil {
			return fmt.Errorf("migrateCorpFleetColumns alter %s.%s: %w", table, column, err)
		}
		return nil
	}
	if err := addColumn("rolebot_gangs", "corpID", "BIGINT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// The fleet doctrine a fleet was raised under (fleetdoctrine.go); empty for a fleet shaped by the composition curve.
	if err := addColumn("rolebot_gangs", "doctrineKind", "VARCHAR(12) NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	// The seat is the spec (fleetdoctrine_seat.go): the hull band, tank layer and range band a member's seat was given at formation.
	// hullMax 0 is a row with no spec.
	for _, col := range [][2]string{
		{"hullMin", "TINYINT NOT NULL DEFAULT 0"},
		{"hullMax", "TINYINT NOT NULL DEFAULT 0"},
		{"tank", "VARCHAR(8) NOT NULL DEFAULT ''"},
		{"style", "TINYINT NOT NULL DEFAULT 0"},
	} {
		if err := addColumn("rolebot_gang_members", col[0], col[1]); err != nil {
			return err
		}
	}
	if err := addColumn("rolebot_corps", "fleetOreStationID", "BIGINT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// What a corporation order is for (corpstock.go): stock (ore the business pass lists), product (the factory's output),
	// store (the company store, sold to members at cost plus a markup).
	if err := addColumn("rolebot_corp_orders", "kind", "VARCHAR(8) NOT NULL DEFAULT 'stock'"); err != nil {
		return err
	}
	// Who placed a corporation order (corporders.go): the CEO, or its
	// Industry Director or deputy when the CEO's slots were full.
	if err := addColumn("rolebot_corp_orders", "placedBy", "BIGINT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// A human seated through the Corporate Portal (fleetmirror.go): purged
	// at boot, because the real gang it sat in died with the process.
	if err := addColumn("rolebot_gang_members", "rider", "TINYINT NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	// Orders from the chair (corpcommanddesk.go): a human's fleet order, directive and work carry their source,
	// and the bot CEO's desks pass over them while they stand.
	for _, c := range []struct{ table, column, ddl string }{
		{"rolebot_corp_fleet_orders", "source", "VARCHAR(8) NOT NULL DEFAULT 'desk'"},
		// A constellation or region in place of one system (corpplace.go).
		{"rolebot_corp_fleet_orders", "areaID", "BIGINT NOT NULL DEFAULT 0"},
		{"rolebot_corp_directive", "setBy", "VARCHAR(8) NOT NULL DEFAULT 'ceo'"},
		{"rolebot_corp_directive", "holdUntil", "BIGINT NOT NULL DEFAULT 0"},
		{"rolebot_corp_directive", "strength", "DOUBLE NOT NULL DEFAULT 0"},
		{"rolebot_corp_work", "postedBy", "BIGINT NOT NULL DEFAULT 0"},
		{"rolebot_corp_work", "source", "VARCHAR(8) NOT NULL DEFAULT 'desk'"},
		// A research or copy order pinned to one original (corpvault.go):
		// two originals of a type work in parallel.
		{"rolebot_corp_work", "bpItemID", "BIGINT NOT NULL DEFAULT 0"},
		{"rolebot_corp_arm_members", "setBy", "VARCHAR(8) NOT NULL DEFAULT 'ceo'"},
		// Departments as customers (corprequest.go): which arm asked for the work, which hangar division it is delivered into (0 = the kind's default),
		// at which station (0 = where built), and the requisition it fills (0 = none).
		{"rolebot_corp_work", "requesterArm", "VARCHAR(16) NOT NULL DEFAULT ''"},
		{"rolebot_corp_work", "destFlag", "BIGINT NOT NULL DEFAULT 0"},
		{"rolebot_corp_work", "destStationID", "BIGINT NOT NULL DEFAULT 0"},
		{"rolebot_corp_work", "reqID", "BIGINT NOT NULL DEFAULT 0"},
		// The division an order was given to, and whether the CEO gave it (corpscope.go).
		{"rolebot_corp_commands", "arm", "VARCHAR(16) NOT NULL DEFAULT ''"},
		{"rolebot_corp_commands", "posterCEO", "TINYINT NOT NULL DEFAULT 0"},
	} {
		if err := addColumn(c.table, c.column, c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// migrateGangLeaderColumns adds leaderCharID/autoFormed/expiresAt to a rolebot_gangs table created before autonomous gang formation.
// Probe once on the first column: the three were added together.
func migrateGangLeaderColumns(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_gangs'
		  AND COLUMN_NAME = 'leaderCharID'`).Scan(&have); err != nil {
		return fmt.Errorf("migrateGangLeaderColumns probe: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE rolebot_gangs
		ADD COLUMN leaderCharID BIGINT NOT NULL DEFAULT 0,
		ADD COLUMN autoFormed   TINYINT NOT NULL DEFAULT 0,
		ADD COLUMN expiresAt    BIGINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrateGangLeaderColumns alter: %w", err)
	}
	return nil
}

// migrateGangEscortColumn adds rolebot_gang_members.recruitCorpID to a table
// created before fleets could second a combat pilot from another corporation.
// The default of 0 reads as "recruited from the gang's own corp", which is
// what every pre-existing row in fact is.
func migrateGangEscortColumn(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_gang_members'
		  AND COLUMN_NAME = 'recruitCorpID'`).Scan(&have); err != nil {
		return fmt.Errorf("migrateGangEscortColumn probe: %w", err)
	}
	if have > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE rolebot_gang_members
		ADD COLUMN recruitCorpID BIGINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("migrateGangEscortColumn alter: %w", err)
	}
	return nil
}

// migrateTraitPolarity negates the stored values of caution, patience and loyalty, whose axes flipped when "hostile end is +100" became the convention;
// resolveTuning flipped with them, so saved pilots behave as authored. One-shot: a marker row at characterID 0 (no bot has ID 0) records that it ran.
func migrateTraitPolarity(ctx context.Context, db *sql.DB) error {
	defer invalidateAllTraits() // writes rolebot_traits (roster.go traits cache)
	var done int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM rolebot_traits
		WHERE characterID = 0 AND trait = '_polarity'`).Scan(&done); err != nil {
		return fmt.Errorf("migrateTraitPolarity probe: %w", err)
	}
	if done > 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE rolebot_traits SET value = -value
		WHERE trait IN (?, ?, ?)`,
		TraitCaution, TraitPatience, TraitLoyalty); err != nil {
		return fmt.Errorf("migrateTraitPolarity flip: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO rolebot_traits (characterID, trait, value) VALUES (0, '_polarity', 1)`); err != nil {
		return fmt.Errorf("migrateTraitPolarity marker: %w", err)
	}
	return nil
}

// migrateActivityChecklist drops rolebot_activities. Runs exactly once: the probe fails once the table is gone.
// Nothing stores a mining disposal any more, so there is nothing to carry across.
func migrateActivityChecklist(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_activities'`,
	).Scan(&have); err != nil || have == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE rolebot_activities`); err != nil {
		return fmt.Errorf("migrateActivityChecklist drop: %w", err)
	}
	return nil
}

// haveTable and haveColumn probe before a migration touches the game's tables (entity, invTypes, staStations), which a scratch schema or an early boot may lack.
// If the game side is missing the migration is skipped whole and its rows left alone, never half-run: a carry skipped with its drop applied loses data.
func haveTable(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table).Scan(&n); err != nil {
		return false, fmt.Errorf("probe table %s: %w", table, err)
	}
	return n > 0, nil
}

// migrateGangOrePerLoad re-keys a pre-existing co-op claims ledger by the hauler carrying the load (the CREATE above has the column). The column is the idempotency probe,
// since a primary-key change is not an ADD COLUMN IF NOT EXISTS. Older rows keep haulerCharID 0, so no hauler can claim them and deleteGang purges them with the fleet.
func migrateGangOrePerLoad(ctx context.Context, db *sql.DB) error {
	ok, err := haveColumn(ctx, db, "rolebot_gang_ore", "haulerCharID")
	if err != nil || ok {
		return err
	}
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE rolebot_gang_ore
			ADD COLUMN haulerCharID BIGINT NOT NULL DEFAULT 0 AFTER gangID,
			DROP PRIMARY KEY,
			ADD PRIMARY KEY (gangID, haulerCharID, characterID, typeID)`); err != nil {
		return fmt.Errorf("migrate rolebot_gang_ore per load: %w", err)
	}
	return nil
}

// ensureLogIndexes adds two read-side keys to rolebot_log, one at a time: each is a long INPLACE build, and a failure must not take module init down.
// (level, logID) serves the error filter; (category, ts) the category filter, the chokes fold and the dropdown. LOCK=NONE keeps the writer flowing.
func ensureLogIndexes(ctx context.Context, db *sql.DB) {
	for _, ix := range []struct{ name, cols string }{
		{"idx_rblog_level", "(level, logID)"},
		{"idx_rblog_cat_ts", "(category, ts)"},
	} {
		have, err := haveIndex(ctx, db, "rolebot_log", ix.name)
		if err != nil {
			rlogf("probe rolebot_log.%s: %v", ix.name, err)
			continue
		}
		if have {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE rolebot_log ADD KEY IF NOT EXISTS `+
			ix.name+` `+ix.cols+`, ALGORITHM=INPLACE, LOCK=NONE`); err != nil {
			rlogf("add rolebot_log.%s: %v (the log pages run without it)", ix.name, err)
		}
	}
}

// ensureMarketIndexes adds the module's own key on the core's market_orders for the station price probes (MAX(price) by station, type, bid, open),
// which had only the stationID key. Non-fatal, INPLACE, LOCK=NONE, like the log's.
func ensureMarketIndexes(ctx context.Context, db *sql.DB) {
	const name = "idx_mo_station_type"
	have, err := haveIndex(ctx, db, "market_orders", name)
	if err != nil {
		rlogf("probe market_orders.%s: %v", name, err)
		return
	}
	if have {
		return
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE market_orders ADD KEY IF NOT EXISTS `+name+
		` (stationID, typeID, bid, orderState, volRemaining, price), ALGORITHM=INPLACE, LOCK=NONE`); err != nil {
		rlogf("add market_orders.%s: %v (the station price probes run without it)", name, err)
	}
}

func haveIndex(ctx context.Context, db *sql.DB, table, index string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		  AND INDEX_NAME = ?`, table, index).Scan(&n); err != nil {
		return false, fmt.Errorf("probe index %s.%s: %w", table, index, err)
	}
	return n > 0, nil
}

func haveColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?
		  AND COLUMN_NAME = ?`, table, column).Scan(&n); err != nil {
		return false, fmt.Errorf("probe column %s.%s: %w", table, column, err)
	}
	return n > 0, nil
}

// haveAllColumns reports whether every named table.column pair is present.
// Pairs are given as "table.column".
func haveAllColumns(ctx context.Context, db *sql.DB, pairs ...string) (bool, error) {
	for _, p := range pairs {
		dot := strings.IndexByte(p, '.')
		if dot < 0 {
			return false, fmt.Errorf("haveAllColumns: %q is not table.column", p)
		}
		ok, err := haveColumn(ctx, db, p[:dot], p[dot+1:])
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// migrateProfileHome moves a pinned rolebot_profiles.homeStationID onto the pilot's clone station
// (character_.homeStationID), then drops the column. The pin wins over the existing clone value.
func migrateProfileHome(ctx context.Context, db *sql.DB) error {
	var have int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'rolebot_profiles'
		  AND COLUMN_NAME = 'homeStationID'`).Scan(&have); err != nil || have == 0 {
		return nil
	}
	// The carry needs somewhere to carry to: without the game's own tables there is no clone column to write,
	// so the migration waits for a boot that has them.
	ready, err := haveAllColumns(ctx, db, "character_.homeStationID")
	if err != nil {
		return err
	}
	if ready {
		if ready, err = haveTable(ctx, db, "staStations"); err != nil {
			return err
		}
	}
	if !ready {
		return nil
	}
	// Only onto pilots on the roster, and only from a station that exists; a pin at a vanished station would leave the clone unresolvable.
	if _, err := db.ExecContext(ctx, `
		UPDATE character_ c
		JOIN rolebot_profiles p ON p.characterID = c.characterID
		JOIN staStations s ON s.stationID = p.homeStationID
		SET c.homeStationID = p.homeStationID
		WHERE p.homeStationID <> 0`); err != nil {
		return fmt.Errorf("migrateProfileHome carry: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE rolebot_profiles DROP COLUMN homeStationID`); err != nil {
		return fmt.Errorf("migrateProfileHome drop: %w", err)
	}
	return nil
}

// migrateBeltDistressCorp re-keys the distress board from (systemID) onto (systemID, corpID) (beltdistress.go). Guarded: it reads the current key first,
// since a bare DROP PRIMARY KEY throws on a keyless table and takes init down. Old rows are deleted, not carried: they have no honest corpID.
func migrateBeltDistressCorp(ctx context.Context, db *sql.DB) error {
	have, err := haveTable(ctx, db, "rolebot_belt_distress")
	if err != nil || !have {
		return err
	}
	// How many columns the current primary key spans. 2 means this has
	// already run; 0 means there is no primary key to drop.
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = 'rolebot_belt_distress'
		  AND INDEX_NAME = 'PRIMARY'`).Scan(&cols); err != nil {
		return fmt.Errorf("migrateBeltDistressCorp read key: %w", err)
	}
	if cols >= 2 {
		return nil
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM rolebot_belt_distress`); err != nil {
		return fmt.Errorf("migrateBeltDistressCorp clear: %w", err)
	}
	if cols == 1 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE rolebot_belt_distress DROP PRIMARY KEY`); err != nil {
			return fmt.Errorf("migrateBeltDistressCorp drop key: %w", err)
		}
	}
	if _, err := db.ExecContext(ctx,
		`ALTER TABLE rolebot_belt_distress ADD PRIMARY KEY (systemID, corpID)`); err != nil {
		return fmt.Errorf("migrateBeltDistressCorp add key: %w", err)
	}
	return nil
}

// migrateWorkClaimsPerJob re-keys rolebot_corp_work_claims by job: keyed on (workID, characterID), a second job on one order overwrote its builder's claim.
// One row per job now, plus at most one unbound (jobID 0) row per pilot. Guarded like migrateBeltDistressCorp; rows survive.
func migrateWorkClaimsPerJob(ctx context.Context, db *sql.DB) error {
	have, err := haveTable(ctx, db, "rolebot_corp_work_claims")
	if err != nil || !have {
		return err
	}
	var cols int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE()
		  AND TABLE_NAME = 'rolebot_corp_work_claims'
		  AND INDEX_NAME = 'PRIMARY'`).Scan(&cols); err != nil {
		return fmt.Errorf("migrateWorkClaimsPerJob read key: %w", err)
	}
	if cols >= 3 {
		return nil
	}
	alter := `ALTER TABLE rolebot_corp_work_claims ADD PRIMARY KEY (workID, characterID, jobID)`
	if cols > 0 {
		alter = `ALTER TABLE rolebot_corp_work_claims DROP PRIMARY KEY, ADD PRIMARY KEY (workID, characterID, jobID)`
	}
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("migrateWorkClaimsPerJob: %w", err)
	}
	return nil
}

// schemaSnapshot is the module's tables' columns and indexes as information_schema
// had them at one moment, keyed "table.name" in lower case. A nil snapshot (the read
// failed) judges nothing, so every ALTER runs as before.
type schemaSnapshot struct {
	cols map[string]bool
	idx  map[string]bool
}

func loadSchemaSnapshot(ctx context.Context, db *sql.DB) *schemaSnapshot {
	snap := &schemaSnapshot{cols: map[string]bool{}, idx: map[string]bool{}}
	load := func(q string, into map[string]bool) bool {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			rlogf("rolebots: schema snapshot: %v (running every ALTER)", err)
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var t, n string
			if rows.Scan(&t, &n) == nil {
				into[strings.ToLower(t+"."+n)] = true
			}
		}
		return rows.Err() == nil
	}
	if !load(`SELECT TABLE_NAME, COLUMN_NAME FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE()`, snap.cols) {
		return nil
	}
	if !load(`SELECT DISTINCT TABLE_NAME, INDEX_NAME FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE()`, snap.idx) {
		return nil
	}
	return snap
}

// alterClauseRe matches a single-clause ALTER whose effect the snapshot can judge.
var alterClauseRe = regexp.MustCompile(`(?is)^\s*ALTER\s+TABLE\s+(\w+)\s+` +
	`(ADD\s+COLUMN\s+IF\s+NOT\s+EXISTS|DROP\s+COLUMN\s+IF\s+EXISTS|` +
	`ADD\s+(?:UNIQUE\s+)?(?:KEY|INDEX)\s+IF\s+NOT\s+EXISTS|DROP\s+(?:KEY|INDEX)\s+IF\s+EXISTS)\s+(\w+)(.*)$`)

// alterApplied reports whether q would change nothing: an ADD ... IF NOT EXISTS of
// something present, or a DROP ... IF EXISTS of something absent. A statement with
// more than one clause (a top-level comma) is never judged.
func (s *schemaSnapshot) alterApplied(q string) bool {
	if s == nil {
		return false
	}
	m := alterClauseRe.FindStringSubmatch(q)
	if m == nil || topLevelComma(m[4]) {
		return false
	}
	key := strings.ToLower(m[1] + "." + m[3])
	verb := strings.ToUpper(strings.Join(strings.Fields(m[2]), " "))
	switch {
	case strings.HasPrefix(verb, "ADD COLUMN"):
		return s.cols[key]
	case strings.HasPrefix(verb, "DROP COLUMN"):
		return !s.cols[key]
	case strings.HasPrefix(verb, "ADD"):
		return s.idx[key]
	case strings.HasPrefix(verb, "DROP"):
		return !s.idx[key]
	}
	return false
}

// topLevelComma reports a comma outside parentheses and quotes.
func topLevelComma(s string) bool {
	depth := 0
	var quote rune
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"' || r == '`':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			return true
		}
	}
	return false
}
