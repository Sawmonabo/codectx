package treesitter

import (
	"bytes"
	"slices"
	"strings"

	"github.com/Sawmonabo/codectx/internal/model"
	"github.com/Sawmonabo/codectx/internal/provider/link"
)

// Spring MVC resolves a handler method's argument that no binding
// annotation marks by the first of its argument resolvers that supports it
// (RequestMappingHandlerAdapter.getDefaultArgumentResolvers): the
// annotation-based ones, of which RequestParamMethodArgumentResolver also
// takes a multipart argument (a MultipartFile or Part, or an array, a
// collection or an Optional of one); the type-based ones (the request, its
// body as an InputStream or Reader, an HttpEntity, and types that are no
// input: the response, the session, the model, a Map, Errors, the locale);
// then the project's own resolvers; then the catch-all, a simple type
// (BeanUtils.isSimpleProperty) as `@RequestParam` keyed by the parameter's
// name, any other as `@ModelAttribute`.

// springTypeInputs are the type-based arguments that read the request, by
// the type's name and the packages that declare it.
var springTypeInputs = map[string]struct {
	src      model.InputSource
	packages []string
}{
	"ServletRequest":              {model.InputRequest, []string{"jakarta.servlet", "javax.servlet"}},
	"HttpServletRequest":          {model.InputRequest, []string{"jakarta.servlet.http", "javax.servlet.http"}},
	"MultipartHttpServletRequest": {model.InputRequest, []string{"org.springframework.web.multipart"}},
	"MultipartRequest":            {model.InputRequest, []string{"org.springframework.web.multipart"}},
	"WebRequest":                  {model.InputRequest, []string{"org.springframework.web.context.request"}},
	"NativeWebRequest":            {model.InputRequest, []string{"org.springframework.web.context.request"}},
	"InputStream":                 {model.InputRaw, []string{"java.io"}},
	"Reader":                      {model.InputRaw, []string{"java.io"}},
	"HttpEntity":                  {model.InputBody, []string{"org.springframework.http"}},
	"RequestEntity":               {model.InputBody, []string{"org.springframework.http"}},
}

// springTypeSkips are the type-based arguments that read no request input,
// by name and package; java.util's Map and its kinds stand for the model.
var springTypeSkips = map[string][]string{
	"ServletResponse": {"jakarta.servlet", "javax.servlet"}, "HttpServletResponse": {"jakarta.servlet.http", "javax.servlet.http"},
	"HttpSession": {"jakarta.servlet.http", "javax.servlet.http"}, "PushBuilder": {"jakarta.servlet.http", "javax.servlet.http"},
	"Principal": {"java.security"}, "HttpMethod": {"org.springframework.http"}, "Locale": {"java.util"},
	"TimeZone": {"java.util"}, "ZoneId": {"java.time"}, "OutputStream": {"java.io"}, "Writer": {"java.io"},
	"RedirectAttributes": {"org.springframework.web.servlet.mvc.support"}, "Model": {"org.springframework.ui"},
	"ModelMap": {"org.springframework.ui"}, "Map": {"java.util"}, "HashMap": {"java.util"},
	"LinkedHashMap": {"java.util"}, "SortedMap": {"java.util"}, "TreeMap": {"java.util"},
	"Errors": {"org.springframework.validation"}, "BindingResult": {"org.springframework.validation"},
	"SessionStatus":        {"org.springframework.web.bind.support"},
	"UriComponentsBuilder": {"org.springframework.web.util"}, "ServletUriComponentsBuilder": {"org.springframework.web.servlet.support"},
}

// springMultipart are the multipart types, by name and package.
var springMultipart = map[string][]string{
	"MultipartFile": {"org.springframework.web.multipart"}, "Part": {"jakarta.servlet.http", "javax.servlet.http"},
}

// springCollections are the collection types a multipart argument may be
// held in (MultipartResolutionDelegate.isMultipartArgument).
var springCollections = map[string]bool{"List": true, "Collection": true, "Set": true, "Iterable": true}

// javaSimple are the java.lang simple value types (ClassUtils
// .isSimpleValueType): the primitives, their wrappers, CharSequence and
// String, Number, Class, and Void excluded.
var javaSimple = map[string]bool{
	"int": true, "long": true, "short": true, "byte": true, "char": true, "boolean": true, "float": true,
	"double": true, "Integer": true, "Long": true, "Short": true, "Byte": true, "Character": true, "Boolean": true,
	"Float": true, "Double": true, "String": true, "CharSequence": true, "Number": true, "Class": true,
	"StringBuilder": true, "StringBuffer": true,
}

// javaSimpleImported are the simple value types of other JDK packages, by
// name and package: Number's BigDecimal and BigInteger, Date, the Temporal
// types, UUID, URI, URL, File, Path, Charset, Currency, InetAddress and
// Pattern (ZoneId, TimeZone and Locale are resolved by type first).
var javaSimpleImported = map[string]string{
	"BigDecimal": "java.math", "BigInteger": "java.math", "Date": "java.util", "UUID": "java.util",
	"Currency": "java.util", "Instant": "java.time", "LocalDate": "java.time", "LocalDateTime": "java.time",
	"LocalTime": "java.time", "OffsetDateTime": "java.time", "OffsetTime": "java.time", "ZonedDateTime": "java.time",
	"Year": "java.time", "YearMonth": "java.time", "URI": "java.net", "URL": "java.net", "InetAddress": "java.net",
	"File": "java.io", "Path": "java.nio.file", "Charset": "java.nio.charset", "Pattern": "java.util.regex",
}

// javaType is a parameter's type as written: its name (the last component
// of a dotted name), the dotted name, its first type argument's name and
// whether it is an array or a variable-arity parameter.
type javaType struct {
	name, dotted, arg string
	array             bool
}

// javaParamType reads the type of a parameter whose tokens after its
// annotations and modifiers are [k, name), name the parameter's own token.
func (s *inputScan) javaParamType(k, name int) (javaType, bool) {
	var t javaType
	first := k
	if !s.ident(k) {
		return t, false
	}
	for s.punct(k+1, '.') && s.ident(k+2) {
		k += 2
	}
	t.name, t.dotted = s.text(k), string(s.b.src[s.toks[first].start:s.toks[k].end])
	k++
	if s.punct(k, '<') {
		depth := 0
		for j := k; j < name; j++ {
			switch {
			case s.punct(j, '<'):
				depth++
				if depth == 1 && t.arg == "" && s.ident(j+1) {
					a := j + 1
					for s.punct(a+1, '.') && s.ident(a+2) {
						a += 2
					}
					t.arg = s.text(a)
				}
			case s.punct(j, '>'):
				depth--
			}
			if depth == 0 {
				k = j + 1
				break
			}
		}
	}
	for k < name {
		switch {
		case s.punct(k, '[') && s.punct(k+1, ']'):
			t.array, k = true, k+2
		case s.punct(k, '.') && s.punct(k+1, '.') && s.punct(k+2, '.'):
			t.array, k = true, k+3
		default:
			return t, false
		}
	}
	return t, true
}

// javaImported reports that the type name, as the file writes it, is the
// type of that name in one of packages: written with the package, imported
// by name, or imported with its package on demand, and declared by no type
// of the file.
func (s *inputScan) javaImported(t javaType, packages []string) bool {
	for _, p := range packages {
		if t.dotted == p+"."+t.name {
			return true
		}
	}
	if t.dotted != t.name || s.classesNamed(t.name) > 0 || s.javaDeclared(t.name) {
		return false
	}
	for _, imp := range s.b.ex.imports {
		for _, p := range packages {
			if imp.Path == p+"."+t.name || imp.Path == p && len(imp.Names) > 0 && imp.Names[0] == "*" {
				return true
			}
		}
	}
	return false
}

// javaDeclared reports a type of the file named name.
func (s *inputScan) javaDeclared(name string) bool {
	for _, i := range s.b.byName[name] {
		switch s.b.decls[i].kind {
		case model.NodeClass, model.NodeInterface, model.NodeEnum, model.NodeStruct:
			return true
		}
	}
	return false
}

// javaImportedByName reports that the file imports a type named name by its
// own name, from any package.
func (s *inputScan) javaImportedByName(name string) bool {
	for _, imp := range s.b.ex.imports {
		if strings.HasSuffix(imp.Path, "."+name) {
			return true
		}
	}
	return false
}

// springDefault reads a handler method's parameter no binding annotation
// marks, under Spring's own order: a multipart argument is a file read keyed
// by its name; a type-based argument that reads the request is published
// as what it reads; any other type-based argument reads nothing. Every
// other argument reaches the catch-all only when the project registers no
// argument resolver of its own, which the linker checks across the unit, so
// it is left to the linker (link.PendingInput, Rule "spring"): a simple
// type the file decides is a combined read keyed by the name, a class the
// file declares a model attribute, and any other type the linker looks up.
func (s *inputScan) springDefault(owner int, p paramRange, k int) {
	name := p.hi - 1
	if !s.ident(name) || name <= k {
		return
	}
	t, ok := s.javaParamType(k, name)
	if !ok {
		return
	}
	param := s.text(name)
	pub := func(src model.InputSource, key string) {
		s.add(fwSpring, src, key, s.toks[p.lo].start, s.toks[p.hi-1].end)
	}
	inner := t
	if t.name == "Optional" && s.javaImported(t, []string{"java.util"}) {
		inner = javaType{name: t.arg, dotted: t.arg}
		if t.arg == "" {
			return
		}
	}
	if pk, ok := springMultipart[inner.name]; ok && s.javaImported(inner, pk) {
		pub(model.InputFile, param)
		return
	}
	if springCollections[inner.name] && s.javaImported(inner, []string{"java.util", "java.lang"}) {
		if pk, ok := springMultipart[inner.arg]; ok && s.javaImported(javaType{name: inner.arg, dotted: inner.arg}, pk) {
			pub(model.InputFile, param)
			return
		}
	}
	if ti, ok := springTypeInputs[t.name]; ok && !t.array && s.javaImported(t, ti.packages) {
		pub(ti.src, "")
		return
	}
	if pk, ok := springTypeSkips[t.name]; ok && !t.array && s.javaImported(t, pk) {
		return
	}
	pending := link.PendingInput{Owner: int32(owner), Framework: fwSpring, Param: param, Rule: link.RuleSpring}
	switch {
	case javaSimple[inner.name] && inner.dotted == inner.name && !s.javaDeclared(inner.name) && !s.javaImportedByName(inner.name):
		pending.Kind = link.ClassSimple
	case javaSimpleImported[inner.name] != "" && s.javaImported(inner, []string{javaSimpleImported[inner.name]}):
		pending.Kind = link.ClassSimple
	case inner != t:
		// An Optional of a type the file does not decide.
		return
	case t.array:
		// An array is simple only of a simple type, which the file
		// decides above.
		return
	default:
		pending.Class = t.dotted
	}
	rng, err := s.b.rangeOf(s.toks[p.lo].start, s.toks[p.hi-1].end)
	if err != nil {
		return
	}
	pending.Range = *rng
	pending.Name = collapse(string(s.b.src[s.toks[p.lo].start:s.toks[p.hi-1].end]))
	s.b.pendingInputs = append(s.b.pendingInputs, pending)
}

// springHandler reports a method Spring maps requests to: one a mapping
// annotation marks, of a class marked `@Controller` or `@RestController`
// (RequestMappingHandlerMapping.isHandler).
func (s *inputScan) springHandler(i int) bool {
	d := &s.b.decls[i]
	if d.kind != model.NodeMethod || d.Parent < 0 {
		return false
	}
	mapped := false
	for _, a := range d.Decorators {
		if _, ok := springMappings[springName(a.Name)]; ok {
			mapped = true
		}
	}
	if !mapped {
		return false
	}
	for _, a := range s.b.decls[d.Parent].Decorators {
		switch a.Name {
		case "org.springframework.stereotype.Controller", springAnnotations + ".RestController":
			return true
		case "Controller":
			if s.javaImported(javaType{name: "Controller", dotted: "Controller"}, []string{"org.springframework.stereotype"}) {
				return true
			}
		case "RestController":
			if s.javaImported(javaType{name: "RestController", dotted: "RestController"}, []string{springAnnotations}) {
				return true
			}
		}
	}
	return false
}

// springValidations are the annotations that leave a parameter to its
// default resolution: Bean Validation's @Valid and Spring's @Validated.
var springValidations = map[string]bool{"jakarta.validation.Valid": true, "javax.validation.Valid": true,
	"org.springframework.validation.annotation.Validated": true}

// springValidation reports a validation annotation as written: with its
// package, or by a name the file imports from it.
func (s *inputScan) springValidation(written string) bool {
	if springValidations[written] {
		return true
	}
	imp, ok := s.rs.imports[written]
	return ok && springValidations[imp.path]
}

// namesArgumentResolver reports a Java file whose bytes name an argument
// resolver: every API a project registers its own resolver through names
// one in its types (WebMvcConfigurer.addArgumentResolvers takes a
// List<HandlerMethodArgumentResolver>; the adapter's setters likewise), so
// a unit where no file does registers none (link.Table.ArgumentResolvers).
func (b *builder) namesArgumentResolver() bool {
	return b.lang.Name == "java" && bytes.Contains(b.src, []byte("ArgumentResolver"))
}

// javaClassKinds is how Spring's catch-all binds a parameter of each type a
// Java file declares (link.ClassKind): an enum is a simple value type, a
// concrete class a model attribute, and an interface, an annotation type or
// an abstract class, which it cannot instantiate, nothing.
func (b *builder) javaClassKinds() []link.ClassKind {
	if b.lang.Name != "java" {
		return nil
	}
	var out []link.ClassKind
	for i := range b.decls {
		d := &b.decls[i]
		kind := link.ClassSkip
		switch d.kind {
		case model.NodeEnum:
			kind = link.ClassSimple
		case model.NodeClass, model.NodeStruct:
			if d.NameStart >= d.Start && d.NameStart <= uint32(len(b.src)) &&
				!slices.Contains(strings.Fields(string(b.src[d.Start:d.NameStart])), "abstract") {
				kind = link.ClassBody
			}
		case model.NodeInterface:
		default:
			continue
		}
		out = append(out, link.ClassKind{Decl: int32(i), Kind: kind})
	}
	return out
}
