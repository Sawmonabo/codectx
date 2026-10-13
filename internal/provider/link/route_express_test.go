package link_test

import "testing"

// TestExpressRoutersAcrossFiles is Express's own layout ("var birds =
// require('./birds'); app.use('/birds', birds)"), in each import form: a
// CommonJS router required and mounted, its handler a function another
// module exports; a default-exported router re-exported by name from an
// index module, which itself mounts a router a third module exports by
// name; a router exported by name and mounted under a path that is no
// literal, whose route has no path. Mutations: "mount unrecorded"
// (expressMount records no import binding) loses every endpoint;
// "re-export not followed" (jsRouters reads no keyReexport) loses /posts/
// and /posts/:id/comments/; "nested mount dropped" (the payload carries no
// mounts) loses /posts/:id/comments/; "imported handler unresolved"
// (handlers reads no Imported) loses /users/'s handler; "unknown prefix
// read as known" (a non-literal first argument kept as a known empty
// prefix) gives the admin route the path /.
func TestExpressRoutersAcrossFiles(t *testing.T) {
	checkMounted(t, map[string]string{
		"package.json": `{"name":"app"}`,
		"app.js": `const express = require("express");
const users = require("./routes/users");
import { posts } from "./routes/index.js";
import { admin } from "./routes/admin.js";
const base = "/admin";
const app = express();
app.use("/users", users);
app.use("/posts", posts);
app.use(base, admin);
app.listen(3000);
`,
		"ctrl.js": `function list(req, res) {}
module.exports = { list };
`,
		"routes/users.js": `const express = require("express");
const ctrl = require("../ctrl");
const router = express.Router();
router.get("/", ctrl.list);
module.exports = router;
`,
		"routes/index.js": `export { default as posts } from "./posts.js";
`,
		"routes/posts.js": `import express from "express";
import { comments } from "./comments.js";
const router = express.Router();
router.get("/", show);
router.use("/:id/comments", comments);
function show(req, res) {}
export default router;
`,
		"routes/comments.js": `import { Router } from "express";
export const comments = Router();
comments.get("/", (req, res) => {});
`,
		"routes/admin.js": `import { Router } from "express";
export const admin = Router();
admin.get("/", (req, res) => {});
`,
	}, []string{
		"app.js GET / /users/ -> list",
		"app.js GET / /posts/ -> show",
		"app.js GET / /posts/:id/comments/ -> ",
		"app.js GET / unresolved -> ",
	})
}
