# Insights

The **Insights** page (a tab next to **Usage & standby**, and `maestria insights`) rates, from 1 to 7, how well agent work on one machine turns AI usage into landed floors over the last 30 days. Every level shows the measure behind it, the threshold for the next level, and one thing to try.

It is a mirror for your own workflow, not a score to compare people. The thresholds are fixed, the same for everyone, and listed below.

## The four areas

| Area | What it measures | Why it matters |
| --- | --- | --- |
| **Delivery speed** | Median time from when a floor starts to when it lands. | Small floors that land fast are cheaper to review and less likely to be thrown away. |
| **Work that holds** | Floors that landed rather than being cancelled. The share of the Maestro's approvals you answered No is shown next to it, but not rated: turning down a risky proposal is good practice. | A cancelled floor is usage that shipped nothing. |
| **Usage that ships** | The share of usage spent while a floor that later landed was being worked on. | This is the return on what you spend. |
| **Your response time** | Median time from when a task needs you (approval, question, choice, agent prompt) to your answer. | Agents wait while a task sits in Needs you. |

The **overall level** is the rounded mean of the rated areas. It needs at least two of them. The area with the lowest level is marked **Start here**.

## Thresholds

A measure that meets the column's value earns that level. Meeting none of them is level 1.

| Area | 7 | 6 | 5 | 4 | 3 | 2 |
| --- | --- | --- | --- | --- | --- | --- |
| Delivery speed (median) | ≤ 4 h | ≤ 8 h | ≤ 24 h | ≤ 48 h | ≤ 4 days | ≤ 7 days |
| Work that holds (floors landed) | ≥ 95% | ≥ 90% | ≥ 80% | ≥ 70% | ≥ 60% | ≥ 50% |
| Usage that ships | ≥ 90% | ≥ 80% | ≥ 70% | ≥ 60% | ≥ 45% | ≥ 30% |
| Your response time (median) | ≤ 5 min | ≤ 15 min | ≤ 30 min | ≤ 1 h | ≤ 3 h | ≤ 8 h |

An area is **not rated yet** until it has enough data in the last 30 days: 3 landed floors (delivery), 3 ended floors (work that holds), 5 percentage points of sampled usage (usage that ships), or 3 answered tasks (response time). The page says what is missing.

## Where the data comes from

Everything is read from the connector's own database on your machine:

- **Floors:** the start and end of each floor, as recorded by `maestria floor start|land|cancel` or by Maestria when a floor disappears from Maestri Wire. Floors marked ongoing (recurring work) are left out: they have no end to deliver.
- **Human tasks:** when each task was posted and answered, and whether an approval was answered Yes or No.
- **Usage samples:** Maestri's usage file only says how much of each window is used *now*. While the panel runs (`maestria serve` or `maestria connect`), the connector stores a sample of each window every 5 minutes and keeps 90 days. `<maestriDir>/usage/providers/` is still only read.

Each usage sample holds the provider id (such as `claude`), a hash of the provider account key (never the key itself, which can be derived from an email address), the window id (such as `session`), the percentage used, the window's reset time and when it was taken. At most 32 accounts and 16 windows per account are sampled. If a sample cannot be stored, the service log says so, at most once an hour (`maestria service logs`).

Nothing is sent anywhere to compute it. In hosted mode the page travels through the relay like every other page, only while you look at it. A machine on a Maestria version from before Insights shows **Update this machine to see Insights** there.

## How usage is placed in time

Between two samples of a window, the rise in its percentage is the usage spent in that interval. When the percentage dropped and the reset time moved on (or it dropped by more than a point), the window reset in between, and what it shows now is all new. A reset time that moves while the percentage keeps rising is not taken for a reset. Each interval is then counted as:

- **shipped**, if a floor that later landed was running;
- **undecided**, and left out, if only floors that are still running (or ongoing floors) were;
- **overhead**, if only cancelled floors, or no floor at all, were running.

Floors that overlap are not split: an interval counts once, as shipped if any floor running then later landed.

For each provider account, the session window is used when there is one, because it moves in finer steps.

## Limits worth knowing

- **Usage is per provider account, not per workspace.** Every workspace on an account shares its windows, so Insights is machine-wide and never splits usage between workspaces.
- **The per-floor figure is an estimate.** "About N% of a session window per landed floor" divides shipped usage by the landed floors. Floors that run at the same time share a window, so it is an average, not a bill. It depends on your plan, so it is shown but not rated.
- **Usage is only seen while the connector runs.** An interval longer than 20 minutes between two samples (the machine slept, the service was stopped) is left out rather than guessed.
- **Postponed tasks count from when they were posted.** Answering clears the postpone, so it cannot be told apart later; the median keeps a few postponed tasks from moving the level.
- **Floors recorded before this feature existed** that vanished from Maestri without being tracked were stored with no duration. They are left out of delivery speed. New ones are recorded from when Maestria first saw them.
- **Prompt quality is not measured.** Maestria does not see the prompts the Maestro sends to its agents, and reading terminals to rate them would change what Maestria keeps about your work.

## From the command line

```
maestria insights          # the same report as text
maestria insights --json   # for scripts
```
