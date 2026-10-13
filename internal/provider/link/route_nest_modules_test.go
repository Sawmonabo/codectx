package link_test

import "testing"

// nestCtrl is a controller file: one class at path serving GET at route.
func nestCtrl(class, path, route, handler string) string {
	return `import { Controller, Get } from "@nestjs/common";

@Controller("` + path + `")
export class ` + class + ` {
  @Get("` + route + `")
  ` + handler + `() {}
}
`
}

// nestModule is a module file: imports, then the module class listing
// controllers.
func nestModule(imports, class, controllers string) string {
	return `import { Module } from "@nestjs/common";
` + imports + `
@Module({ controllers: [` + controllers + `] })
export class ` + class + ` {}
`
}

// TestNestRouterModulePathsJoinBetweenVersionAndController is Nest's
// RouterModule layout (docs, Router module): AppModule registers a route
// tree giving AdminModule `admin` and its children DashboardModule and
// MetricsModule `admin/dashboard` and `admin/metrics`. Each controller's
// routes are served under the path of the module listing it, after the
// global prefix and the version segment and before the controller's own
// path; a module no tree
// names has none, and a controller two modules list is served under both.
// A module lists its controller by a class it declares, an import, or an
// import through a re-exporting index; a same-named controller of another
// file, listed by another module, takes that module's path alone.
// Mutation: the module path not joined -> admin routes without /admin.
// Mutation: the module path joined after the controller's ->
// /api/v1/users/admin.
// Mutation: a child's path not joined to its parent's -> /api/v1/dashboard/...
// Mutation: a listing matched by name alone -> the decoy's /other path on
// the shared controller.
// Mutation: a module's own class not read (Local) -> users route
// unpublished.
// Mutation: the listing's import not resolved through the index's
// re-export -> metrics route unpublished.
// Mutation: a controller served by the first module listing it alone ->
// no /api/v1/shared or no set size.
func TestNestRouterModulePathsJoinBetweenVersionAndController(t *testing.T) {
	checkMounted(t, map[string]string{
		"package.json": `{"name":"app"}`,
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
		"src/app.module.ts": `import { Module } from "@nestjs/common";
import { RouterModule } from "@nestjs/core";
import { AdminModule } from "./admin/admin.module";
import { DashboardModule } from "./admin/dashboard.module";
import { MetricsModule } from "./admin/metrics.module";
import { CatsModule } from "./cats.module";
import { OtherModule } from "./other/other.module";

const routes = [
  {
    path: "admin",
    module: AdminModule,
    children: [
      { path: "dashboard", module: DashboardModule },
      { path: "metrics/", module: MetricsModule },
    ],
  },
  { path: "other", module: OtherModule },
];

@Module({
  imports: [AdminModule, CatsModule, OtherModule, RouterModule.register(routes)],
})
export class AppModule {}
`,
		"src/admin/admin.module.ts": `import { Controller, Get, Module } from "@nestjs/common";

@Controller("users")
class AdminController {
  @Get()
  list() {}
}

@Module({ controllers: [AdminController] })
export class AdminModule {}
`,
		"src/admin/dashboard.controller.ts": nestCtrl("DashboardController", "", "stats", "stats"),
		"src/admin/dashboard.module.ts": nestModule(`import { DashboardController } from "./dashboard.controller";
import { SharedController } from "../shared.controller";
`, "DashboardModule", "DashboardController, SharedController"),
		"src/admin/metrics.controller.ts": nestCtrl("MetricsController", "m", "", "read"),
		"src/admin/index.ts":              `export { MetricsController } from "./metrics.controller";` + "\n",
		"src/admin/metrics.module.ts": nestModule(`import { MetricsController } from "./index";
`, "MetricsModule", "MetricsController"),
		"src/cats.controller.ts":   nestCtrl("CatsController", "cats", "", "findAll"),
		"src/shared.controller.ts": nestCtrl("SharedController", "shared", "", "share"),
		"src/cats.module.ts": nestModule(`import { CatsController } from "./cats.controller";
import { SharedController } from "./shared.controller";
`, "CatsModule", "CatsController, SharedController"),
		"src/other/shared.controller.ts": nestCtrl("SharedController", "o", "", "decoy"),
		"src/other/other.module.ts": nestModule(`import { SharedController } from "./shared.controller";
`, "OtherModule", "SharedController"),
	}, []string{
		`src/admin/admin.module.ts GET "" /api/v1/admin/users -> list`,
		"src/admin/dashboard.controller.ts GET stats /api/v1/admin/dashboard/stats -> stats",
		`src/admin/metrics.controller.ts GET "" /api/v1/admin/metrics/m -> read`,
		`src/cats.controller.ts GET "" /api/v1/cats -> findAll`,
		`src/shared.controller.ts GET "" /api/v1/admin/dashboard/shared -> share set=2`,
		`src/shared.controller.ts GET "" /api/v1/shared -> share set=2`,
		`src/other/shared.controller.ts GET "" /api/v1/other/o -> decoy`,
	})
}

// TestNestRouterModuleTreeAsFlattenRoutePathsReadsIt follows Nest's
// flattenRoutePaths through its corners: a class written as a child takes
// its parent's path; a route object with a module but no path gives the
// module none, and its children join to no parent path; a later entry of
// one module replaces an earlier one.
// Mutation: a child class not given its parent's path -> /cats.
// Mutation: a child object with no path inheriting its parent's -> /a/b/...
// for the grandchild.
// Mutation: the first entry of a module kept over the last -> /x/fish.
func TestNestRouterModuleTreeAsFlattenRoutePathsReadsIt(t *testing.T) {
	checkMounted(t, map[string]string{
		"package.json": `{"name":"app"}`,
		"src/main.ts":  nestMain(""),
		"src/app.module.ts": `import { Module } from "@nestjs/common";
import { RouterModule } from "@nestjs/core";
import { CatsModule } from "./cats.module";
import { BirdsModule } from "./birds.module";
import { DogsModule } from "./dogs.module";
import { FishModule } from "./fish.module";

@Module({
  imports: [
    RouterModule.register([
      { path: "v2", children: [CatsModule] },
      { path: "a", children: [{ module: BirdsModule, children: [{ path: "b", module: DogsModule }] }] },
      { path: "x", module: FishModule },
      { path: "y", module: FishModule },
    ]),
  ],
})
export class AppModule {}
`,
		"src/cats.controller.ts": nestCtrl("CatsController", "cats", "", "findAll"),
		"src/cats.module.ts": nestModule(`import { CatsController } from "./cats.controller";
`, "CatsModule", "CatsController"),
		"src/birds.controller.ts": nestCtrl("BirdsController", "birds", "", "fly"),
		"src/birds.module.ts": nestModule(`import { BirdsController } from "./birds.controller";
`, "BirdsModule", "BirdsController"),
		"src/dogs.controller.ts": nestCtrl("DogsController", "dogs", "", "bark"),
		"src/dogs.module.ts": nestModule(`import { DogsController } from "./dogs.controller";
`, "DogsModule", "DogsController"),
		"src/fish.controller.ts": nestCtrl("FishController", "fish", "", "swim"),
		"src/fish.module.ts": nestModule(`import { FishController } from "./fish.controller";
`, "FishModule", "FishController"),
	}, []string{
		`src/cats.controller.ts GET "" /v2/cats -> findAll`,
		`src/birds.controller.ts GET "" /birds -> fly`,
		`src/dogs.controller.ts GET "" /b/dogs -> bark`,
		`src/fish.controller.ts GET "" /y/fish -> swim`,
	})
}

// TestNestRouterModuleUndecidedLeavesRoutesUnresolved: with RouterModule in
// use, a controller no module lists, a route tree another file holds, a
// module whose controllers are no literal list, and two registrations
// giving one module different paths leave the routes they decide
// unpublished; a controller of a decided module still publishes.
// Mutation: a controller no module lists served with no module path ->
// /cats for "no module".
// Mutation: an imported tree read as empty -> /cats for "imported tree".
// Mutation: an unreadable module ignored -> /cats for "unreadable module".
// Mutation: the first registration kept -> /a/cats for "two
// registrations".
func TestNestRouterModuleUndecidedLeavesRoutesUnresolved(t *testing.T) {
	register := func(tree string) string {
		return `import { Module } from "@nestjs/common";
import { RouterModule } from "@nestjs/core";
import { CatsModule } from "./cats.module";
import { DogsModule } from "./dogs.module";

@Module({ imports: [RouterModule.register(` + tree + `)] })
export class AppModule {}
`
	}
	catsModule := nestModule(`import { CatsController } from "./cats.controller";
`, "CatsModule", "CatsController")
	dogsModule := nestModule(`import { DogsController } from "./dogs.controller";
`, "DogsModule", "DogsController")
	for _, c := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"no module", map[string]string{
			"src/app.module.ts":  register(`[{ path: "d", module: DogsModule }]`),
			"src/cats.module.ts": nestModule("", "CatsModule", ""),
		}, []string{`src/dogs.controller.ts GET "" /d/dogs -> bark`}},
		{"imported tree", map[string]string{
			"src/app.module.ts": `import { Module } from "@nestjs/common";
import { RouterModule } from "@nestjs/core";
import { routes } from "./routes";

@Module({ imports: [RouterModule.register(routes)] })
export class AppModule {}
`,
			"src/routes.ts": "export const routes = [];\n",
		}, nil},
		{"unreadable module", map[string]string{
			"src/app.module.ts": register(`[{ path: "d", module: DogsModule }]`),
			"src/extra.module.ts": `import { Module } from "@nestjs/common";
import { controllers } from "./list";

@Module({ controllers })
export class ExtraModule {}
`,
		}, nil},
		{"two registrations", map[string]string{
			"src/app.module.ts": register(`[{ path: "a", module: CatsModule }, { path: "d", module: DogsModule }]`),
			"src/admin.module.ts": `import { Module } from "@nestjs/common";
import { RouterModule } from "@nestjs/core";
import { CatsModule } from "./cats.module";

@Module({ imports: [RouterModule.register([{ path: "b", module: CatsModule }])] })
export class AdminModule {}
`,
		}, []string{`src/dogs.controller.ts GET "" /d/dogs -> bark`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			files := map[string]string{"package.json": `{"name":"app"}`, "src/main.ts": nestMain(""),
				"src/cats.controller.ts": nestCtrl("CatsController", "cats", "", "findAll"),
				"src/dogs.controller.ts": nestCtrl("DogsController", "dogs", "", "bark"),
				"src/cats.module.ts":     catsModule, "src/dogs.module.ts": dogsModule}
			for p, src := range c.files {
				files[p] = src
			}
			checkMounted(t, files, c.want)
		})
	}
}
