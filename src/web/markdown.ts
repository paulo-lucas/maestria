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
  out = out.replace(/\*\*([^*\n]+?)\*\*/g, '<strong>$1</strong>');
  out = out.replace(/(^|[^\w*])\*([^*\n]+?)\*(?=[^\w*]|$)/g, '$1<em>$2</em>');
  out = out.replace(/(^|[^\w_])_([^_\n]+?)_(?=[^\w_]|$)/g, '$1<em>$2</em>');
  return out;
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
 * Render block + inline Markdown (headings, lists, blockquotes, fenced code, paragraphs)
 * into sanitized HTML. Raw HTML is always escaped, and links are restricted to http/https.
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

  for (const raw of lines) {
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

    const ulMatch = /^\s*[-*]\s+(.*)$/.exec(line);
    if (ulMatch) {
      if (list !== 'ul') {
        closeList();
        out.push('<ul>');
        list = 'ul';
      }
      out.push(`<li>${renderInlineMarkdown(ulMatch[1]!)}</li>`);
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
