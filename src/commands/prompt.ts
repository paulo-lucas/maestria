import readline from 'node:readline';

export type PromptInput = NodeJS.ReadableStream & {
  isTTY?: boolean;
  setRawMode?: (mode: boolean) => void;
  pause?: () => unknown;
  resume?: () => unknown;
};

export interface PromptIO {
  input: PromptInput;
  output: NodeJS.WritableStream;
}

export function defaultPromptIO(): PromptIO {
  return {
    input: process.stdin as PromptInput,
    output: process.stdout,
  };
}

let promptInFlight = false;

/** Discard any chunks already sitting in the readable buffer before showing a prompt. */
export function drainBufferedInput(input: PromptInput): number {
  let dropped = 0;
  if (typeof input.read !== 'function') return 0;
  // Calling pause before read prevents async flowing data races while draining.
  input.pause?.();
  while (((input as { readableLength?: number }).readableLength ?? 0) > 0) {
    const chunk = input.read();
    if (chunk === null) break;
    dropped += Buffer.isBuffer(chunk) ? chunk.length : String(chunk).length;
  }
  // One extra non-blocking read handles streams whose internal buffer needs a pull without readableLength.
  if (dropped === 0 && typeof (input as { readable?: boolean }).readable === 'boolean') {
    let chunk: unknown;
    while ((chunk = input.read()) !== null) {
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

/**
 * Ask a single-line prompt using a dedicated readline interface that is created and closed
 * strictly per question. Only one prompt may be active at a time so stdin listeners/echo
 * never stack.
 */
export function askLine(question: string, io: PromptIO = defaultPromptIO()): Promise<string> {
  if (promptInFlight) {
    return Promise.reject(new Error('Concurrent CLI prompt attempted while another prompt is active'));
  }
  promptInFlight = true;
  drainBufferedInput(io.input);

  return new Promise<string>((resolve, reject) => {
    const rl = readline.createInterface({
      input: io.input,
      output: io.output,
      terminal: Boolean(io.input.isTTY),
    });
    let done = false;
    const finish = (err: Error | null, val?: string) => {
      if (done) return;
      done = true;
      promptInFlight = false;
      rl.close();
      resetTtyBetweenPrompts(io.input);
      if (err) reject(err);
      else resolve(val ?? '');
    };
    rl.once('SIGINT', () => finish(new Error('Prompt cancelled by user')));
    rl.once('close', () => {
      if (!done) finish(new Error('Input stream closed before answer was received'));
    });
    rl.question(question, (answer) => finish(null, answer));
  });
}

/**
 * Ask for sensitive input without echoing characters to output.
 * Uses the same single-prompt lock and closes its interface on completion.
 */
export function askHidden(question: string, io: PromptIO = defaultPromptIO()): Promise<string> {
  if (!io.input.isTTY) {
    return Promise.reject(new Error('A terminal is required to enter the password.'));
  }
  if (promptInFlight) {
    return Promise.reject(new Error('Concurrent CLI prompt attempted while another prompt is active'));
  }
  promptInFlight = true;
  drainBufferedInput(io.input);

  return new Promise<string>((resolve, reject) => {
    io.output.write(question);
    const rl = readline.createInterface({
      input: io.input,
      output: io.output,
      terminal: true,
    });
    (rl as unknown as { _writeToOutput: (s: string) => void })._writeToOutput = () => {};
    let done = false;
    const finish = (err: Error | null, val?: string) => {
      if (done) return;
      done = true;
      promptInFlight = false;
      rl.close();
      resetTtyBetweenPrompts(io.input);
      io.output.write('\n');
      if (err) reject(err);
      else resolve(val ?? '');
    };
    rl.once('SIGINT', () => finish(new Error('Prompt cancelled by user')));
    rl.once('close', () => {
      if (!done) finish(new Error('Input stream closed before answer was received'));
    });
    rl.question('', (answer) => finish(null, answer));
  });
}

/** Prompt repeatedly until `validate` returns a non-null parsed result. Re-asks on empty/invalid input. */
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
