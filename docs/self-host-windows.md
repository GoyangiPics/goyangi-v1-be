# Hosting the Goyangi backend on Windows

Thanks for hosting! This guide sets up the backend on a Windows PC so that it:

- runs in the background as a Windows service, starts with the PC and doesn't need anyone logged in;
- is reachable at `api.goyangi.pics` through a Cloudflare Tunnel, with no port forwarding or router changes;
- **updates itself.** It picks up new code from GitHub, waits until no upload is in progress, restarts on the new version, and goes back to the previous version automatically if the new one fails to start.

The setup is a one-off and takes about 30–45 minutes. After that you shouldn't need to touch it.

All commands are for **PowerShell**. Steps marked *(admin)* need PowerShell opened with *Run as administrator*.

---

## 1. What you get from the owner

The owner sends you these privately, never through a public channel. Keep them private.

| Item | What it is |
|---|---|
| `pb_data.zip` | The database. Contains user accounts and storage keys. |
| `.env` | Settings file with the Discord bot token. |
| Tunnel token | A long string starting with `eyJ…` that connects this PC to `api.goyangi.pics`. |

---

## 2. Install the tools *(admin)*

```powershell
winget install --id Git.Git -e
winget install --id GoLang.Go -e
winget install --id Gyan.FFmpeg -e
winget install --id Cloudflare.cloudflared -e
winget install --id NSSM.NSSM -e
```

- **FFmpeg:** use `Gyan.FFmpeg` (the *full* build), **not** `Gyan.FFmpeg.Essentials`. The server needs encoders that only the full build has.
- **NVIDIA driver:** update it through the NVIDIA app or GeForce Experience. The server uses the GPU's video encoder (NVENC).

Then **restart the PC.** Windows services only see newly installed programs after a reboot.

---

## 3. Get the code

### 3a. Clone into a plain folder

The code is public, so no GitHub account or key is needed. Use a short path **outside OneDrive** (OneDrive syncing breaks the database files):

```powershell
cd C:\
git clone https://github.com/GoyangiPics/goyangi-v1-be.git goyangi
cd C:\goyangi
```

### 3b. Add the files from the owner

- Unzip `pb_data.zip` so that you get a `C:\goyangi\pb_data\` folder containing `data.db`. It should sit directly in `C:\goyangi`, not nested as `pb_data\pb_data`.
- Copy `.env` to `C:\goyangi\.env`. Windows may hide the leading dot or add `.txt`; check with `dir -Force`.

It should look like this:

```
C:\goyangi\
├── .env
├── pb_data\
│   ├── data.db
│   └── auxiliary.db
├── hooks\
├── bot\
├── scripts\
├── main.go
└── ...
```

---

## 4. Test run

```powershell
cd C:\goyangi
go build -o goyangi.exe .
.\goyangi.exe serve --http=127.0.0.1:8090
```

The first build downloads dependencies and takes a minute or two. In the output, look for:

```
🔎 preflight: ffmpeg → C:\...\ffmpeg.exe
🔎 preflight: ffprobe → C:\...\ffprobe.exe
🎬 H.264: NVENC — SD rendition encodes on GPU
✅ preflight ok: ffmpeg/ffprobe found with libsvtav1, libx264, libwebp, aac; H.264 SD on nvenc
🤖 Starting Discord bot...
```

Open http://127.0.0.1:8090/_/ in a browser. You should see the PocketBase admin login, though you don't need to log in.

Then press **Ctrl+C** to stop it.

If you see a ❌ preflight line or `H.264: … CPU`, check [Troubleshooting](#9-troubleshooting) before going on.

---

## 5. Build the updater

```powershell
cd C:\goyangi
go build -o goyangi-updater.exe ./scripts/updater
mkdir logs
```

---

## 6. Install the service *(admin)*

Run `whoami` to get your account name (for example `DESKTOP-ABC\alex`). You need it and your **Windows password** below, so the service runs as you. Use the password, not a PIN; for a Microsoft account, that's the Microsoft account password.

```powershell
nssm install goyangi "C:\goyangi\goyangi-updater.exe"
nssm set goyangi AppParameters "-http 127.0.0.1:8090"
nssm set goyangi AppDirectory "C:\goyangi"
nssm set goyangi ObjectName "DESKTOP-ABC\alex" "your-windows-password"
nssm set goyangi Start SERVICE_AUTO_START
nssm set goyangi AppStdout "C:\goyangi\logs\goyangi.log"
nssm set goyangi AppStderr "C:\goyangi\logs\goyangi.log"
nssm set goyangi AppRotateFiles 1
nssm set goyangi AppRotateOnline 1
nssm set goyangi AppRotateBytes 10485760
nssm set goyangi AppStopMethodConsole 30000
nssm start goyangi
```

Replace `DESKTOP-ABC\alex` and the password with your own.

Check that it's running:

```powershell
nssm status goyangi
Get-Content C:\goyangi\logs\goyangi.log -Tail 30
```

You should see `🛠️ updater: running …` followed by the same preflight lines as in step 4.

> **Why your own account and not the default system account:** the updater runs `git` and `go` in a folder you own, using your Go cache. Under the system account, Git refuses to work in a folder owned by someone else ("dubious ownership").

---

## 7. Connect the tunnel *(admin)*

```powershell
cloudflared service install eyJ...the-token-from-the-owner...
```

That's it. Cloudflared now runs as its own service and starts with Windows. Tell the owner once it's done, and they'll confirm that `https://api.goyangi.pics` answers.

You don't need any firewall or router changes: the tunnel connects outward to Cloudflare. **Don't** change `127.0.0.1` to `0.0.0.0` anywhere; the server shouldn't be reachable directly from your network.

---

## 8. Keep the PC awake *(admin)*

```powershell
powercfg /change standby-timeout-ac 0
powercfg /change hibernate-timeout-ac 0
```

In **Settings → Windows Update → Advanced options → Active hours**, choose hours that suit you, so update reboots happen when you're not using the PC. After a reboot, both services start on their own; you don't need to log in.

---

## How updates work (nothing to do)

Every 2 minutes the updater checks GitHub for a new version. When it finds one, it:

1. builds it in the background while the server keeps running;
2. waits until no uploads or Discord posts are in progress (this can take a while if the queue is busy);
3. restarts the server on the new version;
4. switches back to the previous version automatically if the new one doesn't start properly.

The owner can follow all of this remotely. **Please don't edit files in `C:\goyangi`**, because every update resets the folder to match GitHub. `.env`, `pb_data` and `logs` are never touched.

**The one manual case:** sometimes the owner will tell you *"the updater changed"*. Then run *(admin)*:

```powershell
nssm stop goyangi
cd C:\goyangi
git pull
go build -o goyangi-updater.exe ./scripts/updater
nssm start goyangi
```

---

## 9. Troubleshooting

**Where are the logs?** `C:\goyangi\logs\goyangi.log`. To watch it live:

```powershell
Get-Content C:\goyangi\logs\goyangi.log -Tail 50 -Wait
```

| You see | Meaning and fix |
|---|---|
| `❌ preflight: ffmpeg not found on PATH` | FFmpeg isn't installed, or the PC hasn't been restarted since installing it. Restart, then `nssm restart goyangi`. |
| `❌ preflight: ffmpeg lacks the libsvtav1 encoder` (or another encoder) | You installed the *Essentials* build. Run `winget uninstall Gyan.FFmpeg.Essentials`, install `Gyan.FFmpeg`, then restart the PC. |
| `H.264: … SD rendition encodes on CPU` | The GPU encoder couldn't be used. Update the NVIDIA driver and restart. It still works without the GPU, just slower. |
| `fetch failed: …` (network errors) | Usually a temporary internet or GitHub problem; it retries every 2 minutes. If it keeps happening, check that `git fetch` works when run by hand in `C:\goyangi`. |
| `fetch failed: … dubious ownership` | The service is running as the system account instead of yours. Redo the `ObjectName` line in step 6. |
| `api.goyangi.pics` doesn't load | Run `Get-Service cloudflared`; it should say *Running*. If not, run `Start-Service cloudflared`. |

**Service commands** *(admin)*: `nssm restart goyangi`, `nssm stop goyangi`, `nssm start goyangi`, `nssm status goyangi`.

**Going back to the previous version by hand** (rarely needed, since it normally happens automatically) *(admin)*:

```powershell
nssm stop goyangi
cd C:\goyangi
Copy-Item goyangi.prev.exe goyangi.exe -Force
nssm start goyangi
```

**When something's wrong**, send the owner the last 200 lines of the log:

```powershell
Get-Content C:\goyangi\logs\goyangi.log -Tail 200 | Set-Clipboard
```

---

## Please don't

- **Run a second copy** of the server: no `goyangi.exe` in a terminal while the service is running. Two copies would both answer the Discord bot and process everything twice.
- **Share `.env` or `pb_data`** with anyone. They contain the bot token, storage keys and user data.
- **Expose port 8090** through your router or firewall. The tunnel is the only way in.
