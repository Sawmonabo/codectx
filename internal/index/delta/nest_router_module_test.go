package delta_test

import "testing"

// TestNestRouterModuleEditsRepathTheControllersRoutes is a NestJS unit
// whose controllers take RouterModule paths: AppModule registers a tree
// giving CatsModule `admin`, and CatsModule lists the controllers an index
// re-exports. An edit of the tree's path, of the index's re-export, and of
// the module's list each refresh to what a full build publishes, the
// controllers' routes at their new paths or unresolved; the path edit
// republishes the controller's file.
// Mutation caught: the module listings not traced, so the refresh keeps
// the routes of a controller the module no longer lists.
// Mutation caught: the routers not traced, so the refresh keeps /admin.
func TestNestRouterModuleEditsRepathTheControllersRoutes(t *testing.T) {
	s := newSession(t)
	ctrl := func(class, path string) string {
		return "import { Controller, Get } from \"@nestjs/common\";\n\n@Controller(\"" + path + "\")\n" +
			"export class " + class + " {\n  @Get()\n  findAll() {}\n}\n"
	}
	files := map[string]string{
		"package.json": "{\"name\":\"app\"}\n",
		"src/main.ts": "import { NestFactory } from \"@nestjs/core\";\nimport { AppModule } from \"./app.module\";\n\n" +
			"async function bootstrap() {\n  const app = await NestFactory.create(AppModule);\n  await app.listen(3000);\n}\nbootstrap();\n",
		"src/app.module.ts": "import { Module } from \"@nestjs/common\";\nimport { RouterModule } from \"@nestjs/core\";\n" +
			"import { CatsModule } from \"./cats.module\";\n\n" +
			"@Module({ imports: [CatsModule, RouterModule.register([{ path: \"admin\", module: CatsModule }])] })\n" +
			"export class AppModule {}\n",
		"src/cats.module.ts": "import { Module } from \"@nestjs/common\";\nimport { CatsController, DogsController } from \"./index\";\n\n" +
			"@Module({ controllers: [CatsController, DogsController] })\nexport class CatsModule {}\n",
		"src/index.ts":            "export { CatsController } from \"./cats.controller\";\nexport { DogsController } from \"./dogs.controller\";\n",
		"src/cats.controller.ts":  ctrl("CatsController", "cats"),
		"src/dogs.controller.ts":  ctrl("DogsController", "dogs"),
		"src/other.controller.ts": ctrl("CatsController", "other"),
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "path", replace: []sub{{"src/app.module.ts", "path: \"admin\"", "path: \"staff\""}}}.apply(t, files)
	edited := s.only(s.advance(files))
	s.check("path edited", []linkBuild{edited})
	s.wantRepublished("path edited", edited, []string{"src/app.module.ts", "src/cats.controller.ts",
		"src/dogs.controller.ts"}, "src/other.controller.ts")
	files = edit{name: "reexport", replace: []sub{{"src/index.ts", "from \"./cats.controller\"", "from \"./other.controller\""}}}.apply(t, files)
	s.check("re-export edited", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "list", replace: []sub{{"src/cats.module.ts", "controllers: [CatsController, DogsController]", "controllers: [CatsController]"}}}.apply(t, files)
	s.check("list edited", []linkBuild{s.only(s.advance(files))})
}
