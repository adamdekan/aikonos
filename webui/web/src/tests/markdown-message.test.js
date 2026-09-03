import { describe, it, expect, vi } from "vitest";
import { mount, flushPromises } from "@vue/test-utils";

// Stub vue-stream-markdown so shiki/WASM doesn't run under jsdom.
// The stub passes the content prop through as text — lets us assert
// that MarkdownMessage forwards text→content inside .markdown-message.
vi.mock("vue-stream-markdown", () => ({
  Markdown: {
    props: ["content"],
    template: "<div>{{ content }}</div>",
  },
}));

// CSS import from the library will fail under jsdom; suppress it.
vi.mock("vue-stream-markdown/index.css", () => ({}));

import MarkdownMessage from "../components/MarkdownMessage.vue";

describe("MarkdownMessage", () => {
  it("wraps the markdown renderer and passes text prop through to content", async () => {
    const w = mount(MarkdownMessage, {
      props: { text: "# Hi\n\n**bold** and `code`" },
    });
    await flushPromises();

    // Rendered inside .markdown-message wrapper.
    const wrapper = w.find(".markdown-message");
    expect(wrapper.exists()).toBe(true);

    // Text prop forwarded through the stub renderer.
    const text = wrapper.text();
    expect(text).toContain("bold");
    expect(text).toContain("code");
  });

  it("renders empty without error when text is omitted", async () => {
    const w = mount(MarkdownMessage);
    await flushPromises();
    expect(w.find(".markdown-message").exists()).toBe(true);
  });
});
