# mbidle

`mbidle` ist ein schlanker Hintergrunddienst, der IMAP-Postfächer auf
Änderungen überwacht und bei Signalen vom Server gezielt
[mbsync/isync](https://isync.sourceforge.io/) für genau das betroffene Konto
und den betroffenen Ordner anstößt. Umgekehrt werden lokale Änderungen im
Maildir erkannt und zum Server zurück synchronisiert.

Nachfolger der Ruby/EventMachine-Version (`mbidle` 1.x, Tags `v1.*`): ein
einzelnes Go-Binary, das nicht nur die Inbox, sondern **jeden
synchronisierten Ordner** jedes Kontos beobachtet – mit einer IMAP-Verbindung
pro Konto.

## Funktionsweise

```
            IMAP-Server (je Konto, 1 Verbindung)
        IDLE (INBOX) + LIST-STATUS-Scan │
                                        ▼
 ~/.config/isyncrc ──► mbidle ──► Sync-Queue ──► mbsync [--pull-new|--pull|--push] <channel>[:<ordner>,…] ──► after_sync
                          ▲
       inotify ~/.mail ───┘  (lokale Änderungen)
```

1. **Konfiguration aus mbsync lesen.** Quelle der Wahrheit ist die bestehende
   mbsync-Konfiguration (`$XDG_CONFIG_HOME/isyncrc`, sonst `~/.mbsyncrc`).
   Ausgewertet wird die Kette `IMAPAccount → IMAPStore → Channel (Far) ↔
   MaildirStore (Near)`:
   - Verbindung: `Host`, `Port`, `User`, `TLSType`/`SSLType`,
     `CertificateFile`
   - Passwort: `PassCmd` (z. B. `rbw get …`) bzw. `Pass`
   - zu überwachende Ordner: `Patterns` des Channels (IMAP-Wildcards `*`/`%`,
     Negation `!`, spätere Treffer gewinnen), angewendet auf die
     serverseitige `LIST`. Ordnernamen werden wie bei mbsync kanonisiert
     (Trennzeichen `/`, UTF-8 statt modified UTF-7, Far-Präfix entfernt).
   - lokaler Pfad und Ordner-Mapping: `Path`, `Inbox`, `SubFolders`
     (`Legacy`, `Verbatim`, `Maildir++`), `Flatten`

   Die eigene, optionale Konfiguration enthält nur mbidle-Einstellungen, keine
   Zugangsdaten.

2. **Passwörter über `PassCmd`.** Ausführung beim (Re-)Connect, Ergebnis nur
   im Speicher. Aufrufe sind serialisiert, damit bei gesperrtem `rbw` nur
   *ein* Pinentry-Dialog erscheint. Bei Authentifizierungsfehlern wird der
   Cache verworfen und mit Backoff (10 s … 5 Min.) erneut versucht.

3. **Server beobachten – eine Verbindung pro Konto.** Server begrenzen
   gleichzeitige Verbindungen pro Benutzer/IP (Dovecot
   `mail_max_userip_connections`, Default 10; Gmail 15), mbsync und andere
   Clients zählen mit.

   | Modus | Voraussetzung | Latenz | Verbindungen |
   |---|---|---|---|
   | `idle+scan` (Default bei `auto`) | `IDLE` + `LIST-STATUS` (RFC 5819) | INBOX sofort, Rest ≤ `scan_interval` | 1 |
   | `scan` | – (ohne LIST-STATUS: `STATUS` je Ordner) | ≤ `scan_interval` | 1 |
   | `idle_extra` | `IDLE` | sofort für zusätzlich gewählte Ordner | +1 je Ordner |

   - **IDLE** auf INBOX (read-only per `EXAMINE`): `EXISTS` → neue Mail,
     `EXPUNGE`/`FETCH` → Löschung bzw. Flag-Änderung.
   - **Scan**: Alle `scan_interval` wird IDLE kurz beendet und
     `LIST "" "*" RETURN (STATUS (MESSAGES UIDNEXT UIDVALIDITY UNSEEN HIGHESTMODSEQ))`
     abgesetzt. Das liefert den Zustand *aller* Ordner in einem Roundtrip.
     Abweichungen zum letzten Stand lösen gezielte Syncs aus:
     - nur `UIDNEXT` und `MESSAGES` gleichmäßig gestiegen → nur neue Mails
     - sonst (Löschungen, `UNSEEN`/`HIGHESTMODSEQ` geändert, neue
       `UIDVALIDITY`) → alle Server-Änderungen holen
     - Gmail: `HIGHESTMODSEQ` ist dort kontoweit und wird ignoriert, Flags
       werden über `UNSEEN` (gelesen/ungelesen) erkannt.
   - Der Scan dient zugleich als Keepalive: Kommandos haben ein Timeout, tote
     Verbindungen (Suspend/Resume, Netzwechsel) führen zum Reconnect mit
     Backoff. Nach jedem (Re-)Connect läuft ein voller Sync des Kontos.

4. **Lokale Änderungen erkennen.** Alle Maildir-Verzeichnisse werden per
   inotify beobachtet (`cur/`, `new/`, neue Ordner; ignoriert werden `tmp/`,
   `.mbsyncstate*`, `.uidvalidity`). Änderungen, die mbsync selbst gerade
   schreibt, werden unterdrückt. Der Pfad wird über `Path`/`Inbox`/
   `SubFolders` auf Channel und Ordner zurückgeführt → `mbsync --push`.

5. **Sync-Queue.**
   - Ereignisse werden pro Channel entprellt (`debounce`, Default 2 s),
     dedupliziert und zu einem Aufruf zusammengefasst:
     `mbsync <flags> <channel>:<ordner>,<ordner>…` (ersetzt die `Patterns`,
     mbsync fasst nur diese Ordner an).
   - Flags nach Ereignistyp: nur neue Mails → `--pull-new`; andere
     Server-Änderungen → `--pull`; lokale Änderungen → `--push`; beides →
     voller Sync.
   - Pro Channel läuft höchstens ein mbsync, global höchstens `max_parallel`.
   - Beim Start, nach jedem Reconnect und alle `full_sync` (Default 30 Min.)
     läuft ein voller `mbsync <channel>`, falls Ereignisse verloren gingen.
     Fehlgeschlagene Syncs werden geloggt (inkl. stderr) und spätestens dort
     nachgeholt.

6. **Hook.** Nach erfolgreichen Syncs wird `after_sync` ausgeführt (z. B.
   `notmuch new`), entprellt und nie parallel. Umgebungsvariablen:
   `MBIDLE_CHANNELS` (leerzeichengetrennt), `MBIDLE_FOLDERS`
   (`channel:ordner`, zeilengetrennt; leer bei Vollsync).

## Benutzung

```sh
mbidle check [channel …]     # einloggen, Capabilities, Modus und Ordner anzeigen – ohne Sync
mbidle [-v] [run] [channel …] # Dienst starten (optional nur für einzelne Channels)
mbidle -config other.toml    # andere mbidle-Konfiguration
```

Signale: `SIGUSR1` → sofortiger Vollsync aller Konten; `SIGINT`/`SIGTERM` →
Verbindungen schließen, laufende mbsync-Prozesse zu Ende laufen lassen, aber
höchstens `shutdown_timeout` lang (Default 10 s). Danach bekommen sie
`SIGTERM`, nach 3 s `SIGKILL`, und mbidle beendet sich auch dann, wenn eine
Verbindung ohne Netz noch hängt. Abgebrochene Syncs holt der nächste Vollsync
nach.

## Konfiguration

Optional, `~/.config/mbidle/config.toml` (alle Werte sind Defaults, außer
`after_sync` und den Konto-Einträgen):

```toml
mbsync_config = "~/.config/isyncrc"   # Default: isyncrc, dann ~/.mbsyncrc
mbsync_bin    = "mbsync"
after_sync    = "notmuch new"
debounce      = "2s"
full_sync     = "30m"
max_parallel  = 2
watch_local   = true
shutdown_timeout = "10s"  # Wartezeit auf laufende mbsync beim Beenden

[defaults]
mode          = "auto"   # auto | idle+scan | scan | off
scan_interval = "2m"

[account.work]              # Schlüssel = mbsync-Channel-Name
idle_extra = ["Projects"]   # zusätzliche IDLE-Verbindungen

[account.old]
mode = "off"
```

## Installation (Nix / Home Manager)

```sh
nix build            # ./result/bin/mbidle
nix run . -- check
```

```nix
# flake input: mbidle.url = "github:leoc/mbidle";
systemd.user.services.mbidle = {
  Unit.Description = "mbidle: IMAP watcher for mbsync";
  # graphical-session: rbw braucht für Pinentry die Wayland-/X11-Umgebung
  Unit.After = [ "graphical-session.target" "network-online.target" ];
  Unit.PartOf = [ "graphical-session.target" ];
  Service = {
    ExecStart = "${inputs.mbidle.packages.${pkgs.system}.default}/bin/mbidle";
    Environment = "PATH=${lib.makeBinPath [ pkgs.isync pkgs.rbw pkgs.bash pkgs.coreutils ]}";
    Restart = "on-failure";
    RestartSec = 30;
    TimeoutStopSec = 30;
  };
  Install.WantedBy = [ "graphical-session.target" ];
};
```

## Server-Unterstützung

Getestet mit Dovecot, Gmail und iCloud: alle bieten `IDLE`, `LIST-STATUS`
und `CONDSTORE`, damit läuft überall `idle+scan`. Welche Erweiterungen ein
Server anbietet und welche Ordner beobachtet werden, zeigt `mbidle check`.

## Ideen / offen

- **NOTIFY** (RFC 5465) als Modus `notify`: eine Verbindung meldet Änderungen
  aller Ordner sofort als ungefragte `* STATUS`-Antworten. Lohnt sich, sobald
  mehr Server es anbieten (Dovecot kann es, ist aber selten aktiviert). go-imap
  bietet NOTIFY nicht an, es müsste als Raw-Kommando ergänzt werden.
- Ein `--push` ändert Flags auf dem Server; der nächste Scan erkennt das und
  löst einen (dann leeren) `--pull` des Ordners aus.
- `SIGHUP` zum Neuladen der Konfiguration (bis dahin: Dienst neu starten).
