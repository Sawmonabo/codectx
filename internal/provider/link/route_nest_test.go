package link_test

import "testing"

// nestController is a NestJS controller module: a string path, an array
// route, a method version, and an options controller version-neutral.
const nestController = `import { Controller, Get, Post, Version, VERSION_NEUTRAL } from "@nestjs/common";

@Controller("cats")
export class CatsController {
  @Get()
  findAll() {}

  @Post(["new", "create/"])
  create() {}

  @Version("2")
  @Get(":id")
  findOne() {}
}

@Controller({ path: "health", version: VERSION_NEUTRAL })
export class HealthController {
  @Get()
  check() {}
}
`

// TestNestRoutesJoinTheApplicationsPrefixAndVersion is NestJS's own layout:
// controllers in one file, the application created in main.ts with a global
// prefix and URI versioning. Each route is published at the path Nest's
// RoutePathFactory builds: the version segment (the method's @Version, else
// the controller's, else the default), the controller's path, the route's,
// then the global prefix; a version-neutral controller has no segment.
// Mutation: the global prefix not joined -> paths without /api.
// Mutation: the method's version not preferred to the default -> findOne at
// /api/v1/cats/:id.
// Mutation: VERSION_NEUTRAL read as a version -> a segment in health's path.
// Mutation: the route's own path not joined -> new, create and :id served at
// the controller's path.
func TestNestRoutesJoinTheApplicationsPrefixAndVersion(t *testing.T) {
	checkMounted(t, map[string]string{
		"package.json":           `{"name":"app"}`,
		"src/cats.controller.ts": nestController,
		"src/main.ts": `import { NestFactory } from "@nestjs/core";
import { VersioningType } from "@nestjs/common";
import { AppModule } from "./app.module";

async function bootstrap() {
  const app = await NestFactory.create(AppModule);
  app.setGlobalPrefix("api");
  app.enableVersioning({ type: VersioningType.URI, defaultVersion: "1" });
  await app.listen(3000);
}
bootstrap();
`,
		"src/app.module.ts": "export class AppModule {}\n",
	}, []string{
		`src/cats.controller.ts GET "" /api/v1/cats -> findAll`,
		"src/cats.controller.ts POST new /api/v1/cats/new -> create",
		"src/cats.controller.ts POST create/ /api/v1/cats/create -> create",
		"src/cats.controller.ts GET :id /api/v2/cats/:id -> findOne",
		`src/cats.controller.ts GET "" /api/health -> check`,
	})
}

// nestMain is main.ts creating the application with body as its setup.
func nestMain(body string) string {
	return `import { NestFactory } from "@nestjs/core";
import { AppModule } from "./app.module";

async function bootstrap() {
  const app = await NestFactory.create(AppModule);
` + body + `  await app.listen(3000);
}
bootstrap();
`
}

// TestNestPathsOnlyWhereEveryApplicationIsDecided links one controller
// under the application setups the linker must decide or leave alone. A
// testing application's prefix (createNestApplication) is not read; two
// applications with different prefixes give each route both paths, each
// with the set size. No application in the unit, a prefix set through a
// helper's parameter, a prefix with options (an exclude list matched at
// run time), a prefix that is no literal, and RouterModule in use with no
// module listing the controller publish nothing.
// Mutation: a testing application read as any other receiver -> nothing
// published for "testing application".
// Mutation: a helper's call ignored -> /api/... published for "helper".
// Mutation: options ignored -> /api/... published for "exclude".
// Mutation: RouterModule ignored -> /cats published for "router module".
// Mutation: no application read as no prefix -> /cats published for "no
// application".
// Mutation: two applications' prefixes not a set -> one path, or no
// set size, for "two applications".
func TestNestPathsOnlyWhereEveryApplicationIsDecided(t *testing.T) {
	ctrl := `import { Controller, Get } from "@nestjs/common";

@Controller("cats")
export class CatsController {
  @Get()
  findAll() {}
}
`
	module := "export class AppModule {}\n"
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"testing application", map[string]string{
			"src/main.ts": nestMain("  app.setGlobalPrefix(\"api\");\n"),
			"test/app.e2e-spec.ts": `import { Test } from "@nestjs/testing";
let app;
async function setup(moduleFixture) {
  app = moduleFixture.createNestApplication();
  app.setGlobalPrefix("test");
}
`,
		}, []string{`src/cats.controller.ts GET "" /api/cats -> findAll`}},
		{"no application", map[string]string{}, nil},
		{"helper", map[string]string{
			"src/main.ts":  nestMain("  configure(app);\n"),
			"src/setup.ts": "export function configure(app) {\n  app.setGlobalPrefix(\"api\");\n}\n",
		}, nil},
		{"exclude", map[string]string{
			"src/main.ts": nestMain("  app.setGlobalPrefix(\"api\", { exclude: [\"health\"] });\n"),
		}, nil},
		{"not a literal", map[string]string{
			"src/main.ts": nestMain("  app.setGlobalPrefix(process.env.PREFIX);\n"),
		}, nil},
		{"router module", map[string]string{
			"src/main.ts":       nestMain(""),
			"src/app.module.ts": "import { RouterModule } from \"@nestjs/core\";\nexport class AppModule {}\nexport const routes = RouterModule.register([]);\n",
		}, nil},
		{"two applications", map[string]string{
			"src/main.ts":  nestMain("  app.setGlobalPrefix(\"api\");\n"),
			"src/admin.ts": nestMain("  app.setGlobalPrefix(\"admin\");\n"),
		}, []string{
			`src/cats.controller.ts GET "" /admin/cats -> findAll set=2`,
			`src/cats.controller.ts GET "" /api/cats -> findAll set=2`,
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"package.json": `{"name":"app"}`, "src/cats.controller.ts": ctrl,
				"src/app.module.ts": module}
			for p, src := range c.files {
				files[p] = src
			}
			checkMounted(t, files, c.want)
		})
	}
}
