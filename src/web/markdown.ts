const ESCAPES: Record<string, string> = {
  '&': '&amp;',
  '<': '&lt;',
  '>': '&gt;',
  '"': '&quot;',
  "'": '&#39;',
};

export function escHtml(s: string): string {
  return s.replace(/[&<>"']/g, (c) => ESCAPES[c]!);
}

/** Allow only http/https URLs; reject javascript:, data:, vbscript:, relative/protocol-relative tricks, etc. */
export function safeHttpUrl(raw: string): string | null {
  if (!/^https?:\/\//i.test(raw)) return null;
  try {
    const u = new URL(raw);
    if (u.protocol !== 'http:' && u.protocol !== 'https:') return null;
    return u.href;
  } catch {
    return null;
  }
}

function formatTextSegment(text: string): string {
  let out = escHtml(text);
  out = out.replace(/~~([^~\n]+?)~~/g, '<del>$1</del>');
  out = out.replace(/\*\*([^*\n]+?)\*\*/g, '<strong>$1</strong>');
  out = out.replace(/(^|[^\w*])\*([^*\n]+?)\*(?=[^\w*]|$)/g, '$1<em>$2</em>');
  out = out.replace(/(^|[^\w_])_([^_\n]+?)_(?=[^\w_]|$)/g, '$1<em>$2</em>');
  return out;
}

export type TableAlign = 'left' | 'center' | 'right' | null;

/** Split a GFM table row on unescaped `|` outside backtick code spans. */
export function splitGfmTableRow(line: string): string[] {
  let trimmed = line.trim();
  if (trimmed.startsWith('|')) trimmed = trimmed.slice(1);
  if (trimmed.endsWith('|') && !trimmed.endsWith('\\|')) trimmed = trimmed.slice(0, -1);

  const cells: string[] = [];
  let cur = '';
  let inBacktick = false;

  for (let i = 0; i < trimmed.length; i++) {
    const ch = trimmed[i]!;
    if (ch === '\\' && !inBacktick && trimmed[i + 1] === '|') {
      cur += '|';
      i++;
      continue;
    }
    if (ch === '`') {
      inBacktick = !inBacktick;
      cur += ch;
      continue;
    }
    if (ch === '|' && !inBacktick) {
      cells.push(cur.trim());
      cur = '';
      continue;
    }
    cur += ch;
  }
  cells.push(cur.trim());
  return cells;
}

/** Parse a GFM table delimiter row (e.g. `| --- | :---: | ---: |`). Returns null if invalid. */
export function parseGfmTableAlignments(line: string): TableAlign[] | null {
  const trimmed = line.trim();
  if (!trimmed.includes('|')) return null;
  const cells = splitGfmTableRow(trimmed);
  if (cells.length === 0) return null;
  const aligns: TableAlign[] = [];
  for (const c of cells) {
    if (!/^:?-{2,}:?$/.test(c) && !(trimmed.startsWith('|') && /^:?-{1,}:?$/.test(c))) {
      return null;
    }
    const left = c.startsWith(':');
    const right = c.endsWith(':');
    aligns.push(left && right ? 'center' : right ? 'right' : left ? 'left' : null);
  }
  return aligns;
}

function alignAttr(align: TableAlign | undefined): string {
  if (align === 'center') return ' class="align-center"';
  if (align === 'right') return ' class="align-right"';
  return '';
}

function renderTaskListBody(rawItem: string): { isTask: boolean; html: string } {
  const taskMatch = /^\[( |x|X)\]\s+(.*)$/.exec(rawItem);
  if (!taskMatch) {
    return { isTask: false, html: renderInlineMarkdown(rawItem) };
  }
  const checked = taskMatch[1]!.toLowerCase() === 'x';
  const box = checked
    ? '<span class="task-check checked">✓</span>'
    : '<span class="task-check"></span>';
  return { isTask: true, html: `${box}${renderInlineMarkdown(taskMatch[2]!)}` };
}

/**
 * Render inline Markdown (code, safe links, bold, italic) into sanitized HTML.
 * Raw HTML in the input is always escaped and never interpreted.
 */
export function renderInlineMarkdown(src: string, allowLinks = true): string {
  const tokenRe = allowLinks
    ? /`([^`\n]+)`|\[([^\]]+)\]\(([^)\s]+)\)/g
    : /`([^`\n]+)`/g;
  let out = '';
  let last = 0;
  for (const m of src.matchAll(tokenRe)) {
    const idx = m.index ?? 0;
    if (idx > last) {
      out += formatTextSegment(src.slice(last, idx));
    }
    if (m[1] !== undefined) {
      out += `<code>${escHtml(m[1])}</code>`;
    } else if (m[2] !== undefined && m[3] !== undefined) {
      const safe = safeHttpUrl(m[3]);
      if (safe) {
        out += `<a href="${escHtml(safe)}" target="_blank" rel="noopener noreferrer">${formatTextSegment(m[2])}</a>`;
      } else {
        out += formatTextSegment(m[0]);
      }
    }
    last = idx + m[0].length;
  }
  if (last < src.length) {
    out += formatTextSegment(src.slice(last));
  }
  return out;
}

/**
 * Render block + inline Markdown (headings, GFM tables, lists/task lists, blockquotes,
 * horizontal rules, fenced code, paragraphs) into sanitized HTML.
 * Raw HTML is always escaped, and links are restricted to http/https.
 */
export function renderMarkdown(src: string): string {
  const lines = src.replace(/\r\n?/g, '\n').split('\n');
  const out: string[] = [];
  let list: 'ul' | 'ol' | null = null;
  let inCode = false;
  let codeLines: string[] = [];
  let quoteLines: string[] = [];

  const closeList = () => {
    if (list) {
      out.push(`</${list}>`);
      list = null;
    }
  };
  const closeQuote = () => {
    if (quoteLines.length > 0) {
      out.push(`<blockquote><p>${quoteLines.map((l) => renderInlineMarkdown(l)).join('<br>')}</p></blockquote>`);
      quoteLines = [];
    }
  };

  for (let i = 0; i < lines.length; i++) {
    const raw = lines[i]!;
    if (inCode) {
      if (/^```/.test(raw.trim())) {
        out.push(`<pre><code>${escHtml(codeLines.join('\n'))}</code></pre>`);
        codeLines = [];
        inCode = false;
      } else {
        codeLines.push(raw);
      }
      continue;
    }

    const line = raw.trimEnd();
    if (/^```/.test(line.trim())) {
      closeList();
      closeQuote();
      inCode = true;
      codeLines = [];
      continue;
    }

    if (!line.trim()) {
      closeList();
      closeQuote();
      continue;
    }

    const quoteMatch = /^>\s?(.*)$/.exec(line);
    if (quoteMatch) {
      closeList();
      quoteLines.push(quoteMatch[1]!);
      continue;
    }
    closeQuote();

    const headingMatch = /^(#{1,4})\s+(.*)$/.exec(line);
    if (headingMatch) {
      closeList();
      const tag = headingMatch[1]!.length === 1 ? 'h3' : 'h4';
      out.push(`<${tag}>${renderInlineMarkdown(headingMatch[2]!)}</${tag}>`);
      continue;
    }

    if (/^(?:-{3,}|\*{3,}|_{3,})$/.test(line.trim())) {
      closeList();
      out.push('<hr>');
      continue;
    }

    // GFM pipe table: header row + delimiter row, followed by zero or more body rows.
    if (line.includes('|') && i + 1 < lines.length) {
      const aligns = parseGfmTableAlignments(lines[i + 1]!);
      if (aligns) {
        const headers = splitGfmTableRow(line);
        if (headers.length === aligns.length && headers.length > 0) {
          closeList();
          const theadCells = headers
            .map((h, idx) => `<th${alignAttr(aligns[idx])}>${renderInlineMarkdown(h)}</th>`)
            .join('');
          const rowsHtml: string[] = [];
          let j = i + 2;
          while (j < lines.length) {
            const rowLine = lines[j]!.trimEnd();
            if (!rowLine.trim() || !rowLine.includes('|') || parseGfmTableAlignments(rowLine)) break;
            const cells = splitGfmTableRow(rowLine);
            const tds = aligns
              .map((al, idx) => `<td${alignAttr(al)}>${renderInlineMarkdown(cells[idx] ?? '')}</td>`)
              .join('');
            rowsHtml.push(`<tr>${tds}</tr>`);
            j++;
          }
          const tbody = rowsHtml.length > 0 ? `<tbody>${rowsHtml.join('')}</tbody>` : '';
          out.push(`<div class="md-table-wrap"><table><thead><tr>${theadCells}</tr></thead>${tbody}</table></div>`);
          i = j - 1;
          continue;
        }
      }
    }

    const ulMatch = /^\s*[-*]\s+(.*)$/.exec(line);
    if (ulMatch) {
      if (list !== 'ul') {
        closeList();
        out.push('<ul>');
        list = 'ul';
      }
      const task = renderTaskListBody(ulMatch[1]!);
      out.push(task.isTask ? `<li class="task-item">${task.html}</li>` : `<li>${task.html}</li>`);
      continue;
    }

    const olMatch = /^\s*\d+\.\s+(.*)$/.exec(line);
    if (olMatch) {
      if (list !== 'ol') {
        closeList();
        out.push('<ol>');
        list = 'ol';
      }
      out.push(`<li>${renderInlineMarkdown(olMatch[1]!)}</li>`);
      continue;
    }

    closeList();
    out.push(`<p>${renderInlineMarkdown(line)}</p>`);
  }

  if (inCode) {
    out.push(`<pre><code>${escHtml(codeLines.join('\n'))}</code></pre>`);
  }
  closeQuote();
  closeList();
  return out.join('');
}
