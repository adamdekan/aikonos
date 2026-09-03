import { describe, it, expect } from "vitest";
import { mount } from "@vue/test-utils";
import DataTable from "../components/ui/DataTable.vue";

const COLS = [
  { key: "name", label: "Name" },
  { key: "val",  label: "Value" },
];

const ROWS5 = [
  { name: "a", val: "1" },
  { name: "b", val: "2" },
  { name: "c", val: "3" },
  { name: "d", val: "4" },
  { name: "e", val: "5" },
];

describe("DataTable.vue", () => {
  it("renders one row per item via the row slot", () => {
    const w = mount(DataTable, {
      props: { columns: COLS, rows: ROWS5 },
      slots: {
        row: ({ row }) => `<td>${row.name}</td><td>${row.val}</td>`,
      },
    });
    expect(w.findAll("tbody tr").length).toBe(5);
    expect(w.text()).toContain("a");
    expect(w.text()).toContain("e");
  });

  it("shows EmptyState message when rows=[] and not loading", () => {
    const w = mount(DataTable, {
      props: { columns: COLS, rows: [], loading: false, emptyText: "Nothing here" },
    });
    expect(w.find("table").exists()).toBe(false);
    expect(w.text()).toContain("Nothing here");
  });

  it("shows Spinner when loading=true", () => {
    const w = mount(DataTable, {
      props: { columns: COLS, rows: [], loading: true },
    });
    // Spinner renders a span.spinner
    expect(w.find(".spinner").exists()).toBe(true);
    expect(w.find("table").exists()).toBe(false);
  });

  it("shows only pageSize rows on first page with 5 rows and pageSize=2", () => {
    const w = mount(DataTable, {
      props: { columns: COLS, rows: ROWS5, pageSize: 2 },
      slots: {
        row: ({ row }) => `<td data-testid="cell">${row.name}</td>`,
      },
    });
    expect(w.findAll("tbody tr").length).toBe(2);
    expect(w.text()).toContain("page 1 of 3");
  });

  it("advances to page 2 on Next click and goes back on Prev", async () => {
    const w = mount(DataTable, {
      props: { columns: COLS, rows: ROWS5, pageSize: 2 },
      slots: {
        row: ({ row }) => `<td>${row.name}</td>`,
      },
    });

    // page 1 shows rows a, b
    expect(w.text()).toContain("a");
    expect(w.text()).not.toContain("c");

    await w.find("button:last-of-type").trigger("click"); // Next
    expect(w.text()).toContain("page 2 of 3");
    expect(w.findAll("tbody tr").length).toBe(2);

    await w.find("button:first-of-type").trigger("click"); // Prev
    expect(w.text()).toContain("page 1 of 3");
  });

  it("Prev is disabled on page 1, Next is disabled on last page", async () => {
    const w = mount(DataTable, {
      props: { columns: COLS, rows: ROWS5, pageSize: 2 },
      slots: { row: ({ row }) => `<td>${row.name}</td>` },
    });

    const [prev, next] = w.findAll("button");
    expect(prev.attributes("disabled")).toBeDefined();
    expect(next.attributes("disabled")).toBeUndefined();

    // advance to last page (page 3)
    await next.trigger("click");
    await next.trigger("click");
    expect(w.text()).toContain("page 3 of 3");
    expect(w.findAll("button")[1].attributes("disabled")).toBeDefined();
  });
});
