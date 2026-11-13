import { describe, expect, it } from 'vitest';
import { renderInlineMarkdown, renderMarkdown } from '../src/web/markdown.ts';

describe('markdown sanitizer', () => {
  it('escapes raw HTML and blocks event handler injections', () => {
    const html = renderMarkdown('<script>alert(1)</script>\n\n<img src=x onerror="alert(2)">');
    expect(html).not.toContain('<script>');
    expect(html).not.toContain('<img');
    expect(html).toContain('&lt;script&gt;alert(1)&lt;/script&gt;');
    expect(html).toContain('&lt;img src=x onerror=&quot;alert(2)&quot;&gt;');
  });

  it('blocks dangerous link schemes and allows http/https', () => {
    const js = renderInlineMarkdown('[click](javascript:alert(1))');
    expect(js).not.toContain('<a ');
    expect(js).toContain('javascript:alert(1)');

    const data = renderInlineMarkdown('[click](data:text/html,<script>alert(1)</script>)');
    expect(data).not.toContain('<a ');

    const ok = renderInlineMarkdown('[docs](https://example.com/path?a=1&b=2)');
    expect(ok).toBe('<a href="https://example.com/path?a=1&amp;b=2" target="_blank" rel="noopener noreferrer">docs</a>');
  });

  it('renders headings, lists, blockquotes, fenced code, bold, italic, and inline code', () => {
    const md = [
      '# Title',
      '',
      '- **bold** item with `code_here` and snake_case_name',
      '- _italic_ item',
      '',
      '> quote line',
      '',
      '```',
      'const x = "<tag>";',
      '```',
    ].join('\n');
    const html = renderMarkdown(md);
    expect(html).toContain('<h3>Title</h3>');
    expect(html).toContain('<ul><li><strong>bold</strong> item with <code>code_here</code> and snake_case_name</li><li><em>italic</em> item</li></ul>');
    expect(html).toContain('<blockquote><p>quote line</p></blockquote>');
    expect(html).toContain('<pre><code>const x = &quot;&lt;tag&gt;&quot;;</code></pre>');
  });
});
