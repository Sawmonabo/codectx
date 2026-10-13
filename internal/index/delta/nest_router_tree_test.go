package delta_test

import "testing"

// TestNestImportedTreeAndRenameEditsRepathTheRoutes is a NestJS unit whose
// route tree lives in routes.ts, imported through a barrel, and whose
// module lists its controller by the name another barrel renames it to.
// An edit of the tree's path, of the rename, and of the tree's barrel each
// refresh to what a full build publishes; the path edit republishes the
// controller's file.
// Mutation caught: the tree lookups not traced, so the refresh keeps
// /admin.
// Mutation caught: the rename lookups not traced, so the refresh keeps the
// route of a controller the rename no longer reaches.
func TestNestImportedTreeAndRenameEditsRepathTheRoutes(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"package.json": "{\"name\":\"app\"}\n",
		"src/main.ts": "import { NestFactory } from \"@nestjs/core\";\nimport { AppModule } from \"./app.module\";\n\n" +
			"async function bootstrap() {\n  const app = await NestFactory.create(AppModule);\n  await app.listen(3000);\n}\nbootstrap();\n",
		"src/app.module.ts": "import { Module } from \"@nestjs/common\";\nimport { RouterModule } from \"@nestjs/core\";\n" +
			"import { routes } from \"./routing\";\n\n@Module({ imports: [RouterModule.register(routes)] })\n" +
			"export class AppModule {}\n",
		"src/routing/index.ts": "export { routes } from \"./routes\";\n",
		"src/routing/routes.ts": "import { CatsModule } from \"../cats.module\";\n\n" +
			"export const routes = [{ path: \"admin\", module: CatsModule }];\n",
		"src/cats.module.ts": "import { Module } from \"@nestjs/common\";\nimport { Felines } from \"./pets\";\n\n" +
			"@Module({ controllers: [Felines] })\nexport class CatsModule {}\n",
		"src/pets.ts": "export { CatsController as Felines } from \"./cats.controller\";\n",
		"src/cats.controller.ts": "import { Controller, Get } from \"@nestjs/common\";\n\n@Controller(\"cats\")\n" +
			"export class CatsController {\n  @Get()\n  findAll() {}\n}\n",
		"src/other.controller.ts": "import { Controller, Get } from \"@nestjs/common\";\n\n@Controller(\"other\")\n" +
			"export class OtherController {\n  @Get()\n  findAll() {}\n}\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "path", replace: []sub{{"src/routing/routes.ts", "path: \"admin\"", "path: \"staff\""}}}.apply(t, files)
	edited := s.only(s.advance(files))
	s.check("path edited", []linkBuild{edited})
	s.wantRepublished("path edited", edited, []string{"src/cats.controller.ts", "src/routing/routes.ts"},
		"src/other.controller.ts")
	files = edit{name: "rename", replace: []sub{{"src/pets.ts", "CatsController as Felines", "OtherController as Felines"},
		{"src/pets.ts", "./cats.controller", "./other.controller"}}}.apply(t, files)
	s.check("rename edited", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "barrel", replace: []sub{{"src/routing/index.ts", "export { routes }", "export { routes as unused }"}}}.apply(t, files)
	s.check("barrel edited", []linkBuild{s.only(s.advance(files))})
}
