import { describe, expect, it } from 'vitest';
import { renderInlineMarkdown, renderMarkdown, splitGfmTableRow } from '../src/web/markdown.ts';

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

  it('renders headings, lists, blockquotes, fenced code, bold, italic, strikethrough, and inline code', () => {
    const md = [
      '# Title',
      '',
      '- **bold** item with `code_here` and snake_case_name',
      '- _italic_ item and ~~old~~ text',
      '- [x] Checked task',
      '- [ ] Open task',
      '',
      '---',
      '',
      '> quote line',
      '',
      '```',
      'const x = "<tag>";',
      '```',
    ].join('\n');
    const html = renderMarkdown(md);
    expect(html).toContain('<h3>Title</h3>');
    expect(html).toContain('<li><strong>bold</strong> item with <code>code_here</code> and snake_case_name</li>');
    expect(html).toContain('<li><em>italic</em> item and <del>old</del> text</li>');
    expect(html).toContain('<li class="task-item"><span class="task-check checked">✓</span>Checked task</li>');
    expect(html).toContain('<li class="task-item"><span class="task-check"></span>Open task</li>');
    expect(html).toContain('<hr>');
    expect(html).toContain('<blockquote><p>quote line</p></blockquote>');
    expect(html).toContain('<pre><code>const x = &quot;&lt;tag&gt;&quot;;</code></pre>');
  });

  it('renders GFM tables with alignments, inline formatting, pipes inside code/escapes, and XSS escaping', () => {
    expect(splitGfmTableRow('| a | `b|c` | d\\|e |')).toEqual(['a', '`b|c`', 'd|e']);

    const md = [
      '| Name | Count | Notes |',
      '| :--- | :---: | ---: |',
      '| **Alpha** | `42` | [ref](https://example.com) and `a|b` |',
      '| <script>x</script> | 0 | [bad](javascript:alert(1)) |',
    ].join('\n');
    const html = renderMarkdown(md);
    expect(html).toContain('<div class="md-table-wrap"><table><thead><tr>');
    expect(html).toContain('<th class="align-center">Count</th>');
    expect(html).toContain('<th class="align-right">Notes</th>');
    expect(html).toContain('<td><strong>Alpha</strong></td>');
    expect(html).toContain('<td class="align-center"><code>42</code></td>');
    expect(html).toContain('<a href="https://example.com/" target="_blank" rel="noopener noreferrer">ref</a> and <code>a|b</code>');
    expect(html).toContain('&lt;script&gt;x&lt;/script&gt;');
    expect(html).not.toContain('javascript:alert(1)">');
  });
});
