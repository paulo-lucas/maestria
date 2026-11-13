export class ArgError extends Error {}

export interface FlagSpec {
  type: 'boolean' | 'string';
  short?: string;
}

export interface ParsedArgs {
  flags: Record<string, string | boolean>;
  positional: string[];
}

export function parseArgs(argv: readonly string[], spec: Record<string, FlagSpec>): ParsedArgs {
  const flags: Record<string, string | boolean> = {};
  const positional: string[] = [];
  const shortMap = new Map<string, string>();
  for (const [name, s] of Object.entries(spec)) {
    if (s.short) shortMap.set(s.short, name);
  }

  let endOfFlags = false;
  for (let i = 0; i < argv.length; i++) {
    const arg = argv[i]!;
    if (endOfFlags) {
      positional.push(arg);
      continue;
    }
    if (arg === '--') {
      endOfFlags = true;
      continue;
    }
    if (arg.startsWith('--')) {
      const eq = arg.indexOf('=');
      const rawName = eq >= 0 ? arg.slice(2, eq) : arg.slice(2);
      const def = Object.hasOwn(spec, rawName) ? spec[rawName] : undefined;
      if (!def) throw new ArgError(`Unknown flag: --${rawName}`);
      if (def.type === 'boolean') {
        if (eq >= 0) throw new ArgError(`Flag --${rawName} does not take a value`);
        flags[rawName] = true;
      } else {
        let val: string | undefined;
        if (eq >= 0) {
          val = arg.slice(eq + 1);
        } else {
          i += 1;
          val = argv[i];
        }
        if (val === undefined) throw new ArgError(`Flag --${rawName} requires a value`);
        flags[rawName] = val;
      }
      continue;
    }
    if (arg.startsWith('-') && arg.length > 1) {
      const short = arg.slice(1);
      const longName = shortMap.get(short);
      if (!longName) throw new ArgError(`Unknown flag: -${short}`);
      const def = spec[longName]!;
      if (def.type === 'boolean') {
        flags[longName] = true;
      } else {
        i += 1;
        const val = argv[i];
        if (val === undefined) throw new ArgError(`Flag -${short} requires a value`);
        flags[longName] = val;
      }
      continue;
    }
    positional.push(arg);
  }

  return { flags, positional };
}
