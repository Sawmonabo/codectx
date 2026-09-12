import { readFile as rf } from "fs";

// héllo → 日本
export const greeting: string = "héllo → 日本";

export interface Handler {
  serve(name: string): Promise<void>;
  port: number;
}

/** A server class. */
export class Server implements Handler {
  port = 8080;
  async serve(name: string): Promise<void> {
    const inner = (): string => helper(name);
    inner();
  }
}

export enum Mode { Fast = 1, Slow = 2 }

function helper(name: string): string {
  return greeting + rf(name);
}
