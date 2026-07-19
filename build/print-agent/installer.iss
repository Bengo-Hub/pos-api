; Inno Setup script for the Codevertex POS Print Agent.
; Built by .github/workflows/print-agent-release.yml on a windows-latest runner:
;   go build -o build\print-agent\print-agent.exe .\cmd\print-agent
;   ISCC.exe build\print-agent\installer.iss
; Produces Output\CodevertexPrintAgentSetup.exe which installs the exe and registers + starts a
; Windows service (CodevertexPrintAgent) that auto-starts on boot and serves 127.0.0.1:9330.
;
; UPGRADE CONTRACT (the duplicate-agent race guard): before ANY file is copied, the previous
; installation is fully torn down — service stopped, stray print-agent.exe processes killed,
; the SCM registration removed, and the old binary deleted — then the new version installs and
; registers FRESH. Exactly one agent process/service can exist on the machine afterwards.
; The pairing config (agent.json) is deliberately PRESERVED across upgrades so the reinstalled
; agent resumes the SAME server-side identity (print_agents row) — wiping it would force a
; re-pair that mints a second pairing, which is precisely the two-live-agents race this exists
; to prevent. A true UNINSTALL purges the pairing too (print-agent.exe purge-config).

#define AppVersion GetEnv("AGENT_VERSION")
#if AppVersion == ""
  #define AppVersion "1.4.0"
#endif

#define ServiceName "CodevertexPrintAgent"

[Setup]
AppId={{B8F1B7E2-9C3A-4E1D-9F2B-CODEVERTEXPA01}
AppName=Codevertex POS Print Agent
AppVersion={#AppVersion}
AppPublisher=Codevertex Africa Limited
DefaultDirName={autopf}\Codevertex\PrintAgent
DefaultGroupName=Codevertex Print Agent
DisableProgramGroupPage=yes
OutputDir=Output
OutputBaseFilename=CodevertexPrintAgentSetup
Compression=lzma2
SolidCompression=yes
PrivilegesRequired=admin
ArchitecturesInstallIn64BitMode=x64
UninstallDisplayName=Codevertex POS Print Agent
WizardStyle=modern
; The [Code] teardown handles running processes explicitly; never rely on Restart Manager
; (services aren't "applications" to it) and never end setup asking for a reboot.
CloseApplications=no
RestartIfNeededByRun=no

[Files]
Source: "print-agent.exe"; DestDir: "{app}"; Flags: ignoreversion

[Run]
; Register + start the background service (auto-start on boot). The [Code] section already
; guaranteed no previous registration exists, so `install` cannot collide with a stale entry.
Filename: "{app}\print-agent.exe"; Parameters: "install"; Flags: runhidden waituntilterminated; StatusMsg: "Registering the print service..."
Filename: "{app}\print-agent.exe"; Parameters: "start"; Flags: runhidden waituntilterminated; StatusMsg: "Starting the print service..."

[UninstallRun]
; Stop + remove the service, then wipe the pairing config from every profile it may live in
; (uninstall = clean slate; the next install must start unpaired).
Filename: "{app}\print-agent.exe"; Parameters: "stop"; Flags: runhidden waituntilterminated; RunOnceId: "StopPrintAgent"
Filename: "{app}\print-agent.exe"; Parameters: "uninstall"; Flags: runhidden waituntilterminated; RunOnceId: "RemovePrintAgent"
Filename: "{app}\print-agent.exe"; Parameters: "purge-config"; Flags: runhidden waituntilterminated; RunOnceId: "PurgePrintAgentConfig"

[UninstallDelete]
; Remove anything left beside the exe (legacy agent.json fallback, logs) so the app dir goes away.
Type: filesandordirs; Name: "{app}"

[Messages]
SetupWindowTitle=Codevertex POS Print Agent Setup

[Code]
function ScCmd(const Args: String): Integer;
var
  R: Integer;
begin
  if Exec(ExpandConstant('{sys}\sc.exe'), Args, '', SW_HIDE, ewWaitUntilTerminated, R) then
    Result := R
  else
    Result := -1;
end;

function ServiceRegistered(): Boolean;
begin
  { sc query exits 0 when the service exists (any state), 1060 when it doesn't. }
  Result := ScCmd('query {#ServiceName}') = 0;
end;

{ Tear down any existing installation completely: stop service -> kill strays -> unregister
  -> delete the old binary. Runs BEFORE [Files], so the new exe never fights a locked file
  and [Run] install always registers against a clean SCM. Every step is best-effort — a
  half-broken previous install (deleted exe but live SCM entry, or vice versa) must never
  block the fresh install. }
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  OldExe: String;
  R, I: Integer;
begin
  Result := '';

  { 1. Stop the running service and wait for it to leave RUNNING state. }
  if ServiceRegistered() then
  begin
    ScCmd('stop {#ServiceName}');
    for I := 0 to 19 do
    begin
      Sleep(500);
      { RUNNING/STOP_PENDING both keep the exe alive; taskkill below is the backstop. }
      if ScCmd('query {#ServiceName}') <> 0 then Break;
      { No reliable state parse from exit codes — bounded wait, then force-kill. }
    end;
  end;

  { 2. Kill ANY leftover agent process — a hung service host, an interactively-launched
       agent, or a second copy someone started by hand. This is what guarantees the
       "never more than one agent" invariant on this machine after setup. }
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/f /im print-agent.exe', '', SW_HIDE, ewWaitUntilTerminated, R);
  Sleep(500);

  { 3. Unregister: prefer the old exe's own uninstall (kardianos removes the SCM entry
       cleanly), then `sc delete` as the backstop for orphaned/half-registered entries. }
  OldExe := ExpandConstant('{app}\print-agent.exe');
  if FileExists(OldExe) then
    Exec(OldExe, 'uninstall', '', SW_HIDE, ewWaitUntilTerminated, R);
  if ServiceRegistered() then
    ScCmd('delete {#ServiceName}');

  { 4. SCM deletion is async when handles linger — wait until the entry is truly gone so
       the [Run] `install` step can never race a marked-for-deletion service. }
  for I := 0 to 19 do
  begin
    if not ServiceRegistered() then Break;
    Sleep(500);
  end;

  { 5. Clean the old binary; [Files] then lays the new one down fresh. Pairing config
       (agent.json under the service profile) is intentionally NOT touched here. }
  DeleteFile(OldExe);
end;
