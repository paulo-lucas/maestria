import readline from 'node:readline';

export type PromptInput = NodeJS.ReadableStream & {
  isTTY?: boolean;
  readableLength?: number;
  isPaused?: () => boolean;
  setRawMode?: (mode: boolean) => void;
  pause?: () => unknown;
  resume?: () => unknown;
};

export interface WatchdogEvent {
  question: string;
  wasPaused: boolean;
  questionPrinted: boolean;
}

export interface PromptIO {
  input: PromptInput;
  output: NodeJS.WritableStream;
  /** Override silent-wait watchdog delay (ms); 0 disables. Defaults to 15_000 ms. */
  watchdogMs?: number;
  onWatchdog?: (event: WatchdogEvent) => void;
}

export const DEFAULT_WATCHDOG_MS = 15_000;

export function defaultPromptIO(): PromptIO {
  return {
    input: process.stdin as PromptInput,
    output: process.stdout,
  };
}

let promptInFlight = false;

/** For tests/diagnostics: whether a prompt currently holds the single-prompt lock. */
export function isPromptInFlight(): boolean {
  return promptInFlight;
}

/**
 * Discard bytes already sitting in the stream's readable buffer before showing a prompt.
 * Never calls `.read()` speculatively on an empty TTY stream (which can leave it in pull mode).
 */
export function drainBufferedInput(input: PromptInput): number {
  let dropped = 0;
  if (typeof input.read !== 'function') return 0;
  input.pause?.();

  if (typeof input.readableLength === 'number') {
    let reads = 0;
    while (input.readableLength > 0 && reads < 64) {
      reads += 1;
      const chunk = input.read();
      if (chunk === null) break;
      dropped += Buffer.isBuffer(chunk) ? chunk.length : String(chunk).length;
    }
    return dropped;
  }

  // Fallback for minimal non-TTY test streams without readableLength.
  if (!input.isTTY) {
    const chunk = input.read();
    if (chunk !== null) {
      dropped += Buffer.isBuffer(chunk) ? chunk.length : String(chunk).length;
    }
  }
  return dropped;
}

function resetTtyBetweenPrompts(input: PromptInput): void {
  if (input.isTTY && typeof input.setRawMode === 'function') {
    try {
      input.setRawMode(false);
    } catch {
      /* ignore */
    }
  }
  input.pause?.();
}

function runSinglePrompt(opts: {
  question: string;
  hidden: boolean;
  io: PromptIO;
}): Promise<string> {
  const { question, hidden, io } = opts;

  if (hidden && !io.input.isTTY) {
    return Promise.reject(new Error('A terminal is required to enter the password.'));
  }
  if (promptInFlight) {
    return Promise.reject(new Error('Concurrent CLI prompt attempted while another prompt is active'));
  }
  promptInFlight = true;

  return new Promise<string>((resolve, reject) => {
    let rl: readline.Interface | null = null;
    let done = false;
    let sawInput = false;
    let questionPrinted = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    const onData = () => {
      sawInput = true;
    };
    const onInputClose = () => {
      cleanupAndFinish(new Error('Input stream closed before answer was received'));
    };
    const onInputError = (err: Error) => {
      cleanupAndFinish(err);
    };

    const cleanupAndFinish = (err: Error | null, val?: string) => {
      if (done) return;
      done = true;
      if (timer) {
        clearTimeout(timer);
        timer = null;
      }
      io.input.removeListener?.('data', onData);
      io.input.removeListener?.('close', onInputClose);
      io.input.removeListener?.('error', onInputError);
      try {
        rl?.close();
        resetTtyBetweenPrompts(io.input);
        if (hidden) io.output.write('\n');
      } finally {
        promptInFlight = false;
      }
      if (err) reject(err);
      else resolve(val ?? '');
    };

    try {
      drainBufferedInput(io.input);

      rl = readline.createInterface({
        input: io.input,
        output: io.output,
        terminal: hidden ? true : Boolean(io.input.isTTY),
      });

      const rlAny = rl as unknown as { _writeToOutput?: (s: string) => void };
      if (hidden) {
        rlAny._writeToOutput = () => {};
      } else {
        const origWrite = rlAny._writeToOutput;
        rlAny._writeToOutput = (chunk: string) => {
          if (question.length > 0 && chunk.includes(question)) {
            questionPrinted = true;
          }
          origWrite?.call(rl, chunk);
        };
      }

      rl.once('SIGINT', () => cleanupAndFinish(new Error('Prompt cancelled by user')));
      rl.once('close', () => {
        if (!done) cleanupAndFinish(new Error('Input stream closed before answer was received'));
      });
      io.input.on?.('data', onData);
      io.input.once?.('close', onInputClose);
      io.input.once?.('error', onInputError);

      if (hidden) {
        if (question.length > 0) {
          io.output.write(question);
          questionPrinted = true;
        }
        io.input.resume?.();
        rl.resume();
        rl.question('', (answer) => cleanupAndFinish(null, answer));
      } else {
        io.input.resume?.();
        rl.resume();
        rl.question(question, (answer) => cleanupAndFinish(null, answer));
        if (!questionPrinted && question.length > 0) {
          io.output.write(question);
          questionPrinted = true;
        }
      }

      const watchdogMs = io.watchdogMs ?? DEFAULT_WATCHDOG_MS;
      if (watchdogMs > 0) {
        timer = setTimeout(() => {
          if (done || sawInput) return;
          const wasPaused = Boolean(io.input.isPaused?.());
          io.input.resume?.();
          rl?.resume();
          const alreadyPrinted = questionPrinted;
          if (!alreadyPrinted && question.length > 0) {
            io.output.write(question);
            questionPrinted = true;
          } else if (wasPaused && question.length > 0) {
            io.output.write(`\n${question}`);
          }
          io.onWatchdog?.({ question, wasPaused, questionPrinted: alreadyPrinted });
        }, watchdogMs);
        timer.unref?.();
      }
    } catch (err) {
      cleanupAndFinish(err instanceof Error ? err : new Error(String(err)));
    }
  });
}

/**
 * Ask a single-line prompt using a dedicated readline interface that is created and closed
 * strictly per question. Only one prompt may be active at a time so stdin listeners/echo
 * never stack, and stdin is always resumed after draining buffered input.
 */
export function askLine(question: string, io: PromptIO = defaultPromptIO()): Promise<string> {
  return runSinglePrompt({ question, hidden: false, io });
}

/**
 * Ask for sensitive input without echoing characters to output.
 * Uses the same single-prompt lock and closes its interface on completion.
 */
export function askHidden(question: string, io: PromptIO = defaultPromptIO()): Promise<string> {
  return runSinglePrompt({ question, hidden: true, io });
}

/** Prompt repeatedly until `validate` returns an ok parsed result. Re-asks on empty/invalid input. */
export async function askValidated<T>(
  question: string,
  validate: (raw: string) => { ok: true; value: T } | { ok: false; error: string },
  io: PromptIO = defaultPromptIO(),
): Promise<T> {
  while (true) {
    const raw = await askLine(question, io);
    const check = validate(raw);
    if (check.ok) return check.value;
    io.output.write(`${check.error}\n`);
  }
}

export function askYesNo(
  question: string,
  opts: { defaultLabel: string; io?: PromptIO },
): Promise<boolean> {
  const io = opts.io ?? defaultPromptIO();
  return askValidated<boolean>(
    question,
    (raw) => {
      const v = raw.trim().toLowerCase();
      if (v === 'y' || v === 'yes') return { ok: true, value: true };
      if (v === 'n' || v === 'no') return { ok: true, value: false };
      return { ok: false, error: `Please enter "y" (yes) or "n" (no) [default: ${opts.defaultLabel}].` };
    },
    io,
  );
}

export function askKeepOrRedo(
  question: string,
  io: PromptIO = defaultPromptIO(),
): Promise<'keep' | 'redo'> {
  return askValidated<'keep' | 'redo'>(
    question,
    (raw) => {
      const v = raw.trim().toLowerCase();
      if (v === 'keep' || v === 'k' || v === 'n' || v === 'no') return { ok: true, value: 'keep' };
      if (v === 'redo' || v === 'r' || v === 'y' || v === 'yes') return { ok: true, value: 'redo' };
      return { ok: false, error: 'Please enter "keep" or "redo" (recommended: keep).' };
    },
    io,
  );
}
