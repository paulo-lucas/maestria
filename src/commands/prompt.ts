import { Writable } from 'node:stream';
import { checkbox, confirm, input, password, select } from '@inquirer/prompts';

export type PromptInput = NodeJS.ReadableStream & {
  isTTY?: boolean;
  readableLength?: number;
  isPaused?: () => boolean;
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

/**
 * Discard bytes already sitting in the stream's readable buffer before rendering a prompt,
 * so stray buffered keypresses/newlines cannot accidentally accept or skip a question.
 */
export function drainBufferedInput(inputStream: PromptInput): number {
  let dropped = 0;
  if (typeof inputStream.read !== 'function') return 0;
  inputStream.pause?.();

  if (typeof inputStream.readableLength === 'number') {
    let reads = 0;
    while (inputStream.readableLength > 0 && reads < 64) {
      reads += 1;
      const chunk = inputStream.read();
      if (chunk === null) break;
      dropped += Buffer.isBuffer(chunk) ? chunk.length : String(chunk).length;
    }
    return dropped;
  }

  if (!inputStream.isTTY) {
    const chunk = inputStream.read();
    if (chunk !== null) {
      dropped += Buffer.isBuffer(chunk) ? chunk.length : String(chunk).length;
    }
  }
  return dropped;
}

class NonClosingOutput extends Writable {
  private readonly target: NodeJS.WritableStream;
  constructor(target: NodeJS.WritableStream) {
    super();
    this.target = target;
  }
  override _write(chunk: Buffer | string, _encoding: BufferEncoding, cb: (error?: Error | null) => void): void {
    this.target.write(chunk, cb);
  }
  override end(..._args: unknown[]): this {
    return this;
  }
  get isTTY(): boolean {
    return Boolean((this.target as { isTTY?: boolean }).isTTY);
  }
  get columns(): number {
    return (this.target as { columns?: number }).columns ?? 80;
  }
  get rows(): number {
    return (this.target as { rows?: number }).rows ?? 24;
  }
}

function prepareContext(io: PromptIO): { input: NodeJS.ReadableStream; output: NodeJS.WritableStream } {
  drainBufferedInput(io.input);
  io.input.resume?.();
  const output =
    io.output === process.stdout || io.output === process.stderr ? io.output : new NonClosingOutput(io.output);
  return { input: io.input, output };
}

export async function askLine(
  message: string,
  io: PromptIO = defaultPromptIO(),
  opts?: { defaultValue?: string; validate?: (value: string) => boolean | string | Promise<boolean | string> },
): Promise<string> {
  return input(
    {
      message: message.trimEnd(),
      default: opts?.defaultValue,
      validate: opts?.validate,
    },
    prepareContext(io),
  );
}

export async function askHidden(message: string, io: PromptIO = defaultPromptIO()): Promise<string> {
  if (!io.input.isTTY) {
    throw new Error('A terminal is required to enter the password.');
  }
  return password(
    {
      message: message.trimEnd(),
      mask: '*',
    },
    prepareContext(io),
  );
}

export async function askConfirm(
  message: string,
  opts: { defaultValue?: boolean; io?: PromptIO } = {},
): Promise<boolean> {
  const io = opts.io ?? defaultPromptIO();
  return confirm(
    {
      message: message.trimEnd(),
      default: opts.defaultValue ?? true,
    },
    prepareContext(io),
  );
}

export async function askSelect<T extends string>(
  message: string,
  choices: Array<{ name: string; value: T; description?: string }>,
  opts: { defaultValue?: T; io?: PromptIO } = {},
): Promise<T> {
  const io = opts.io ?? defaultPromptIO();
  return select<T>(
    {
      message: message.trimEnd(),
      choices,
      default: opts.defaultValue,
    },
    prepareContext(io),
  );
}

export async function askCheckbox<T extends string>(
  message: string,
  choices: Array<{ name: string; value: T; checked?: boolean }>,
  io: PromptIO = defaultPromptIO(),
): Promise<T[]> {
  return checkbox<T>(
    {
      message: message.trimEnd(),
      choices,
    },
    prepareContext(io),
  );
}

export async function askKeepOrRedo(
  message: string,
  io: PromptIO = defaultPromptIO(),
  labels: { keep?: string; redo?: string } = {},
): Promise<'keep' | 'redo'> {
  return askSelect<'keep' | 'redo'>(
    message,
    [
      { name: labels.keep ?? 'Keep existing (recommended)', value: 'keep' },
      { name: labels.redo ?? 'Redo / reconfigure', value: 'redo' },
    ],
    { defaultValue: 'keep', io },
  );
}
