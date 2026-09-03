import pino from "pino";

export const log = pino({
  level: process.env.LOG_LEVEL ?? "info",
  transport:
    process.env.NODE_ENV === "production"
      ? undefined
      : { target: "pino/file", options: { destination: 2 } }, // stderr, readable
});

export type Logger = typeof log;
