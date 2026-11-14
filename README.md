# GM-Rolebots

RoleBots is a module for the GuriMoon server (the Red Moon Rising, 2005-2006
EVE Online restoration server). It fills the universe with persistent,
believable pilots who live in it the way players do: they mine, run missions
and open sites, hunt bounties, haul and trade, build and research, found and
run corporations and alliances, fight wars and sieges, and talk in local,
corp and alliance chat and on the Intergalactic Summit.

Bots are ordinary characters on ordinary accounts. Each one has a home, a
personality and a wallet, buys its ships and modules from the real market,
and is simulated in the abstract when nobody is around and flown in space
when a player can see it.

## Requirements

- A working GuriMoon server checkout: database imported, server builds and
  boots.
- A server version that carries the core seams this module plugs into. An
  older server fails to compile with the module (a compile error, never a
  silent drop); update the server first.
- Go, the same toolchain the server builds with.

The module needs no SQL import, no `config.json` edit and no extra Go
dependency.

## Install

Clone this repository into the server's `modules` folder, then rebuild the
server from its root:

```bat
cd <server repo>\modules
git clone <this repo> GM-Rolebots
cd ..
rebuild.bat
gurimoon.exe
```

On Linux or macOS use `./rebuild.sh` instead of `rebuild.bat`.

- **Build from the server repo root.** `rebuild.bat` finds every folder in
  `modules\` that has a `mod-rebuild.ps1` and compiles it in; the folder
  name does not matter. `rebuild.bat check` compiles with the module without
  touching the live `gurimoon.exe`.
- **The first boot creates the module's tables** (`rolebot_*`, with
  `CREATE TABLE IF NOT EXISTS`). Nothing to import.

## Turning it on

The bots start **off**: a fresh build carries a dormant module.

1. Open the admin panel (port 26080) and go to **Rolebot → Bot Population**.
2. Press **Switch RoleBot ON**.
3. Use **RoleBot Generation** (`/rolebots/seed`) to create a population of
   generated pilots. Existing player characters can be enrolled from the
   **Main Cast** page instead.

The **Rolebot** section of the admin panel holds the roster, each pilot's
profile and fit, the activity log, trade hubs, factories, corporations,
alliances and the chatter settings.

The online cap counts players too: as people log in, bots log off idlest
first (never one a player can currently see) and come back as seats free.

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
| `enabled` | `false` | Master switch default before the first toggle in the admin panel. After that the panel's switch is what counts. |

## Optional companions

- **SCC stock exchange module** (`SCC-mod`): when it sits beside this module
  in `modules\`, the bots trade shares on the exchange. Without it they trade
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

## Layout

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
