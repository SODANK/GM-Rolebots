# Rolebots

*A GuriMoon module*

Rolebots is a module that populates your server with bots that progress just
like you do and take part in the economy and industry from the ground up. They
can also manage a corporation's finances, science, industry, structures and war
efforts.

The name Rolebots reflects a priority: automated player characters that respect
EVE's setting and atmosphere, rather than ones that try to replicate true player
behavior. Many player tactics and strategies are built into their logic, but in
how they present themselves, Rolebots act like people of New Eden instead of
gamers who are just visiting.

To use Rolebots, drop the repo into GuriMoon's `modules` folder and rebuild the
server. You can generate Rolebots on the ESP tool website, in the new
**Rolebot** category, on the **Bot Population** page. The rest of the ESP tool
treats Rolebots as real players, so they show up elsewhere in ESP as well. The
Rolebot category is specifically for managing them and looking into their stats
and behavior.

## Features

### True persistence

- Rolebots can be generated to start out in a school corporation with starting
  skills and work their way up from day 1. Alternatively, more experienced
  Rolebots can be seeded into the game world with a history in the various NPC
  corporations.
- Rolebots support a **Main Cast** of bots. You can expand it by creating a
  character as normal on a standard account and handing it over to the module,
  which puppets it like any generated bot. You can take your character back at
  any time, and you can even adopt a generated bot and have it placed on your
  account.
- Rolebots that are not currently seen by a player's client exist in the
  **Abstract**, which simulates a Rolebot's travel and actions elsewhere in the
  game world. Simple math simulates what happens in the background, with the
  proper skill percentages applied to their encounters.
- Time zones can be enforced or not, at your preference. Certain bots will only
  appear online during certain parts of the day, based on your computer's own
  clock.

### Personality traits

- Rolebots hold scores in several traits that build their psyche and influence
  their decisions and choices: which activities they're drawn to, how they pick
  their ships and modules, how they react to another Rolebot's heritage, and how
  they would lead a corporation, if they have it in them at all. Everything has
  a tag, and a Rolebot's scores sway it toward decisions another Rolebot might
  not make.

### Activities

- Miners gather resources to put on the market, industrialists use them to work
  off the BPOs they acquire and make products for the market or their
  corporation, and traders arbitrage and flip what gets made. Meanwhile, combat
  pilots seek out danger by killing NPCs in belts or hunting rivals in other
  corporations.
- Bots can run missions and dungeons. Combined with the ability to create your
  own, a drop-in system lets them run and clear your sites, so long as a site
  checks out with their reasoning before they warp in.

### Gangs

- Rolebots form gangs and tackle content together. They form mining fleets and
  combat patrols to hunt belt NPCs or run dungeons, or commit acts of piracy
  against you or other bots.
- Players can recruit Rolebots into fleets, as long as they don't offend a
  condition or personality trait the Rolebot has. Recruited Rolebots follow
  commands given in gang chat.

### Wealth and assets

- The only ship a bot can materialize is its rookie ship, just like you.
  Otherwise, it has to use the market or industry to get new ships. No module,
  round of ammo or digit of ISK is magically spawned in: Rolebots must engage
  with the world to obtain anything beyond their starting seed.
- They obtain real BPOs, research and copy them, and build from them.
- Bots request courier services, so check the board to move some cargo!
- Bots use escrow to trade goods such as fitted ships or researched copies of
  their BPOs.

### Chatter

- Rolebots make quips and commentary in the chat channels you share with them,
  about the events and actions they're a part of. Pick up clues about trade
  items, or witness smack talk in Local over a fight elsewhere in the system.
- Rolebots chat in corporation, alliance and gang channels, giving insight into
  what's happening.
- They even post on the Galnet browser's Intergalactic Summit, giving context on
  the ongoing drama between them for players to read.

### Corporation and alliance management

- Rolebots that are CEOs of a corporation recruit, expand their operations and
  build a cabinet of directors to lead specialized arms of their organization.
  A CEO's personality traits shape the corporation's direction and focus, as
  well as its diplomacy with other corporations.
- Alliances work, but because they're gated by both skills and money, don't
  expect one to form naturally anytime soon. Instead, use the ESP tool to create
  alliances between corporations. They'll take care of the rest.
- Rolebot corporations can declare war on each other and even hire mercenaries
  to attack rivals on their behalf. Mutual wars can emerge and last for months
  at a time.
- Rolebot corporations manage POS structures, seek out lucrative moon resources
  to help finance the corporation, and pursue sovereignty over nullsec systems.
  Rival corporations may even lay siege to them.
- On Galnet's **Corporate Portal** page, a player can see work orders and other
  ways to help a corporation they belong to that is run by a Rolebot CEO.

## Getting started

Drop the module into the `modules` folder of your GuriMoon server and run
`rebuild.bat` to build the server with the Rolebots module. Then open your ESP
web page and find the new **Rolebot** category near the bottom. The first
option, **Bot Population**, is where you want to be. Its first page introduces
your Main Cast, if you have one; the next sub-page is the random generator,
where you can create bots en masse to populate the game world.

Several options shape the kind of bots you want in the world. More
industrialists? More miners? Combat specialists? You can decide all of that
here, as well as the demographic makeup of the bots' bloodlines and the time
zones they keep.

Bot skill levels come in three categories: **Green**, **Seasoned** and
**Veteran**. Green bots start their journey inside NPC schools and begin in
those systems like any new player. Seasoned and Veteran bots are generated
elsewhere in the world, employed by NPC corporations, with generated employment
histories that suggest they've been around for much longer.

Because bots progress in real time, it may take many hours of running the
server to see their progress grow. You can use the ESP tool to check active
industry jobs in the world.

## Requirements

- A working GuriMoon server checkout: database imported, server builds and
  boots.
- A server version that carries the core seams this module plugs into. An
  older server fails to compile with the module (a compile error, never a
  silent drop); update the server first.
- Go, the same toolchain the server builds with.

The module needs no SQL import, no `config.json` edit and no extra Go
dependency. The first boot creates its `rolebot_*` tables.

**The bots start off.** A fresh build carries a dormant module: on the
**Bot Population** page, press **Switch RoleBot ON** before generating bots.

On Linux or macOS, use `./rebuild.sh` instead of `rebuild.bat`.
`rebuild.bat check` compiles with the module without touching the live
`gurimoon.exe`.

## Configuration

Everything is optional. Add a block under `"modules"` in the server's
`config.json`:

```json
"modules": {
  "rolebots": {
    "console": true,
    "consoleMirror": false,
    "simWorkers": 8,
    "offloadMinutes": 10
  }
}
```

| Key | Default | Meaning |
|---|---|---|
| `console` | `true` | Open the module's own log window (Windows) so bot activity stays out of the server console. Other systems print to the server console. |
| `consoleMirror` | `false` | Also echo every module line to the server console. |
| `simWorkers` | `8` | How many bots take their simulation turn at once. `1` runs them one at a time. |
| `offloadMinutes` | `10` | How often combat pilots offload loot. |
| `enabled` | `false` | Master switch default before the first toggle in the admin panel. After that, the panel's switch is what counts. |

## Optional companions

- **SCC stock exchange module** (`SCC-mod`): when it sits beside this module
  in `modules\`, the bots trade shares on the exchange. Without it, they trade
  shares at the treasury desk only.

## Removing it

Run `rebuild.bat core`, or put an empty file named `.disabled` in this
folder and rebuild. The `rolebot_*` tables stay in the database, unused, and
are picked up again if the module is built back in.

## How it builds

The module is compiled in with `go build -overlay`, which only **adds**
files to the build. It rewrites no file in the server repository:

1. `modules/modules.go` → a generated file that imports the module.
2. `modules/rolebots/**` → the `rolebots/` source tree in this repository.
3. `core/**/*_mod.go` → small bridge files added to core packages, which
   connect the module to the server's nil-by-default seams.

`mod-rebuild.ps1` (Windows) and `mod-rebuild.sh` (POSIX) write that overlay
map. The server's root `rebuild` script calls them for you. Run them
directly only to build this module on its own:

```bat
mod-rebuild.bat          :: build the server with this module only
mod-rebuild.bat check    :: compile check, output in build\
```

`-ServerRepo` (or `--server-repo`) defaults to two folders up, which is the
server repository when this folder sits in `<server repo>\modules\`.

| Path | Contents |
|---|---|
| `rolebots/` | The module source (package `gurimoon/modules/rolebots`). |
| `core/` | Bridge files the overlay adds to core server packages. |
| `mod-rebuild.*` | Overlay build scripts. |
| `build/` | Generated at build time (overlay map, check executable). Not committed. |

## License

GNU Affero General Public License v3.0. See [LICENSE](LICENSE), the same
text as the GuriMoon server this module compiles into.

The AGPL's network clause applies to the combined server: if you run a
modified version and let other people connect to it, you must offer those
users the source of your modified version. This license covers the code in
this repository only and grants no rights in EVE Online, its client, its
assets or its trademarks.
