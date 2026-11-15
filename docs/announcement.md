# Launch post (draft)

A short post for an AI or Maestri users channel. Replace `<REPO_URL>` with the repository address. The image links assume the files are served from the repository. If your channel needs attachments, upload the files from `docs/images/` named in each link.

---

## Maestria: answer your AI agents from your phone

Your agents work for hours. Then they stop and ask: *"Merge this into main?"* or *"Which database should I use?"*, and wait until you are back at your desk.

**Maestria** is a free, open-source control panel for [Maestri](https://www.themaestri.app) that puts those questions in your pocket. You see what each agent is doing, answer in one tap, and the work goes on.

![The overview: every workspace, and what needs you](<REPO_URL>/raw/main/docs/images/overview.png)

### What it does

- **Needs you.** Approvals, questions and choices from all your workspaces in one list. Approve, reject, answer or pick.
- **Live activity and chat.** See which agent is doing what, and talk to your Maestro, through Maestri Wire.
- **Floors.** What is planned, in progress and landed, with progress.
- **Usage and standby.** It watches your AI limits and pauses the work before you hit them, then wakes your Maestro when the limit resets.

![Human tasks on a phone](<REPO_URL>/raw/main/docs/images/mobile-tasks.png)

### Your data stays yours

- It runs on **your machine**. The panel listens on `127.0.0.1` only. Nothing is opened to the internet.
- Reach it from your phone with a free **Cloudflare Tunnel**, or through a **hosted server** that only relays: it stores none of your workspace data and cannot forge a command.
- Sign-in needs a password **and** an authenticator code. A **kill switch** cuts off remote access, and only someone at your machine can turn it back on.

![Where things run: your machine holds the data and the rules](<REPO_URL>/raw/main/docs/images/architecture-hosted.svg)

### Start in two commands

```sh
curl --proto '=https' --tlsv1.2 -fsSL https://raw.githubusercontent.com/paulo-lucas/maestria/main/install.sh | sh
maestria init
```

Then open <http://127.0.0.1:4777>. On Windows, use the PowerShell line from the README. The wizard explains each question, and the guides do not assume you are used to the terminal.

![Floors, on your phone](<REPO_URL>/raw/main/docs/images/mobile-floors.png)

### Learn more

- Repository and README: <REPO_URL>
- Getting started: <REPO_URL>/blob/main/docs/getting-started.md
- Security in plain words: <REPO_URL>/blob/main/docs/security.md

It is MIT licensed and young. Tell us what is confusing, what is missing and what you would use it for. Issues and questions are welcome.
