package delta_test

import "testing"

// TestNestPrefixEditRepathsTheControllersRoutes is a NestJS controller
// whose routes take the global prefix main.ts sets: an edit of the prefix
// refreshes to what a full build publishes, the controller's routes at
// their new paths, and republishes the controller's file; an edit of a file
// no route reads republishes no endpoint file. Mutation caught: the
// application lookup not traced, so the refresh keeps the stale paths.
func TestNestPrefixEditRepathsTheControllersRoutes(t *testing.T) {
	s := newSession(t)
	files := map[string]string{
		"package.json": "{\"name\":\"app\"}\n",
		"src/main.ts": "import { NestFactory } from \"@nestjs/core\";\nimport { AppModule } from \"./app.module\";\n\n" +
			"async function bootstrap() {\n  const app = await NestFactory.create(AppModule);\n  app.setGlobalPrefix(\"api\");\n" +
			"  await app.listen(3000);\n}\nbootstrap();\n",
		"src/app.module.ts": "export class AppModule {}\n",
		"src/cats.controller.ts": "import { Controller, Get } from \"@nestjs/common\";\n\n@Controller(\"cats\")\n" +
			"export class CatsController {\n  @Get()\n  findAll() {}\n}\n",
		"src/util.ts": "export function one() {\n  return two();\n}\nfunction two() {\n  return 2;\n}\n",
	}
	s.check("base", []linkBuild{s.only(s.advance(files))})
	files = edit{name: "prefix", replace: []sub{{"src/main.ts", "setGlobalPrefix(\"api\")", "setGlobalPrefix(\"v2\")"}}}.apply(t, files)
	edited := s.only(s.advance(files))
	s.check("prefix edited", []linkBuild{edited})
	s.wantRepublished("prefix edited", edited, []string{"src/cats.controller.ts", "src/main.ts"}, "src/util.ts")
	files = edit{name: "unrelated", replace: []sub{{"src/util.ts", "return 2", "return 3"}}}.apply(t, files)
	unrelated := s.only(s.advance(files))
	s.check("unrelated edit", []linkBuild{unrelated})
	s.wantRepublished("unrelated edit", unrelated, []string{"src/util.ts"}, "src/cats.controller.ts")
}
