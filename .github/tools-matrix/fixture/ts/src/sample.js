import { readFile as rf } from "fs";
const path = require("path");

// Non-ASCII before the declarations: héllo → 日本
export const greeting = "héllo → 日本";

/** A server class. */
export class Server {
  port = 8080;
  start(name) {
    const inner = () => helper(name);
    return inner();
  }
}

function helper(name) {
  return path.join(greeting, rf(name));
}

it("starts the server", () => new Server().start("x"));
export { helper };
