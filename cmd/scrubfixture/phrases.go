package main

import (
	"strconv"
	"strings"
)

// Lang is one of the languages the phrase table covers, by ISO 639-1 code.
type Lang string

// The languages the phrase table covers.
const (
	LangEnglish Lang = "en"
	LangFrench  Lang = "fr"
	LangSpanish Lang = "es"
	LangDutch   Lang = "nl"
)

// englishMarkers are the two lines every English description starts with, so the remote
// gate and the evidence code in later tasks have something real to hit.
var englishMarkers = []string{
	"Fully remote within the EU.",
	"Experience with Go and PostgreSQL.",
}

// phraseSet is a small job-ad vocabulary in one language, written for these fixtures.
type phraseSet struct {
	headings  []string
	sentences []string
}

// phrases holds our own job-ad sentences per language. They are deliberately ordinary
// sentences so a language detector reads them as the language they are in.
//
//nolint:misspell // non-English phrase table: the English-only speller misreads French, Spanish and Dutch words
var phrases = map[Lang]phraseSet{
	LangEnglish: {
		headings: []string{
			"About the role", "What you will do", "What we are looking for", "Nice to have", "What we offer",
		},
		sentences: []string{
			"You will design and maintain backend services in Go.",
			"You will work closely with product managers and other engineers to ship features every week.",
			"We expect strong experience with distributed systems and cloud infrastructure.",
			"You will review code, write tests and help improve our deployment pipeline.",
			"Our team values clear communication, ownership and steady delivery.",
			"You have at least five years of experience building web applications.",
			"Knowledge of Kubernetes and observability tooling is a plus.",
			"We offer flexible working hours and a yearly learning budget.",
			"You will mentor other engineers and take part in technical planning.",
			"The role includes on-call duty for the services you build, shared fairly across the team.",
		},
	},
	LangFrench: {
		headings: []string{
			"À propos du poste", "Vos missions", "Profil recherché", "Les plus", "Ce que nous offrons",
		},
		sentences: []string{
			"Vous concevrez et maintiendrez des services backend en Go.",
			"Vous travaillerez avec les chefs de produit et les autres ingénieurs pour livrer de nouvelles fonctionnalités chaque semaine.",
			"Nous attendons une solide expérience des systèmes distribués et de l'infrastructure cloud.",
			"Vous relirez le code, écrirez des tests et aiderez à améliorer notre chaîne de déploiement.",
			"Notre équipe valorise une communication claire, le sens des responsabilités et la régularité.",
			"Vous avez au moins cinq ans d'expérience dans le développement d'applications web.",
			"La connaissance de Kubernetes et des outils d'observabilité est un atout.",
			"Nous proposons des horaires flexibles et un budget annuel de formation.",
			"Vous accompagnerez d'autres ingénieurs et participerez à la planification technique.",
			"Le poste est ouvert en télétravail complet depuis l'Union européenne.",
		},
	},
	LangSpanish: {
		headings: []string{
			"Sobre el puesto", "Qué harás", "Qué buscamos", "Se valorará", "Qué ofrecemos",
		},
		sentences: []string{
			"Diseñarás y mantendrás servicios backend en Go.",
			"Trabajarás junto a responsables de producto y otros ingenieros para entregar nuevas funcionalidades cada semana.",
			"Buscamos experiencia sólida con sistemas distribuidos e infraestructura en la nube.",
			"Revisarás código, escribirás pruebas y ayudarás a mejorar nuestro proceso de despliegue.",
			"Nuestro equipo valora la comunicación clara, la responsabilidad y la constancia.",
			"Tienes al menos cinco años de experiencia desarrollando aplicaciones web.",
			"Se valorará el conocimiento de Kubernetes y de herramientas de observabilidad.",
			"Ofrecemos horario flexible y un presupuesto anual para formación.",
			"Acompañarás a otros ingenieros y participarás en la planificación técnica.",
			"El puesto es en remoto completo desde la Unión Europea.",
		},
	},
	LangDutch: {
		headings: []string{
			"Over de functie", "Wat ga je doen", "Wat wij zoeken", "Pluspunten", "Wat wij bieden",
		},
		sentences: []string{
			"Je ontwerpt en onderhoudt backenddiensten in Go.",
			"Je werkt samen met productmanagers en andere engineers om elke week nieuwe functies op te leveren.",
			"Wij verwachten ruime ervaring met gedistribueerde systemen en cloudinfrastructuur.",
			"Je beoordeelt code, schrijft tests en helpt onze uitrolstraat te verbeteren.",
			"Ons team waardeert heldere communicatie, eigenaarschap en een constant tempo.",
			"Je hebt minstens vijf jaar ervaring met het bouwen van webapplicaties.",
			"Kennis van Kubernetes en observability-tools is een pluspunt.",
			"Wij bieden flexibele werktijden en een jaarlijks opleidingsbudget.",
			"Je begeleidt andere engineers en denkt mee over de technische planning.",
			"De functie is volledig op afstand binnen de Europese Unie.",
		},
	},
}

// companyNames is the fixed fictional company list. Real companies are keyed onto it in
// the order they are first seen; past the end, names repeat with a number.
var companyNames = []string{
	"Northwind Labs", "Contoso Systems", "Fabrikam Software", "Tailspin Data", "Wingtip Cloud",
	"Litware Analytics", "Proseware Tech", "Woodgrove Digital", "Lucerne Engineering", "Adatum Platforms",
	"Coho Networks", "Alpine Ski Works", "Trey Research", "Blue Yonder Labs", "Margie Travel Tech",
}

// syntheticLocation replaces every location.
const syntheticLocation = "Exampleville, Country"

// syntheticTitle is the job title for synthetic job number n.
func syntheticTitle(n int) string {
	return "Senior Platform Engineer " + strconv.Itoa(n)
}

// slugify turns a fictional name into a URL slug ("Northwind Labs" -> "northwind-labs").
func slugify(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), "-"))
}

// uiText is fixed interface wording that carries nothing about a job, a company or a
// person, so it is kept as it is. Everything else is replaced.
var uiText = map[string]bool{
	"Apply": true, "Easy Apply": true, "Save": true, "Show more": true, "Show less": true,
	"Sign in": true, "Join now": true, "Report this job": true, "Email or phone": true,
	"Password": true, "Show": true, "Hide": true, "Forgot password?": true, "or": true,
	"Sign in with Email": true, "Dismiss": true, "New to LinkedIn?": true,
	"User Agreement": true, "Privacy Policy": true, "Cookie Policy": true,
	"Seniority level": true, "Employment type": true, "Job function": true, "Industries": true,
	"No longer accepting applications": true, "Base pay range": true,
}

// criteriaValues are the fixed values LinkedIn uses for two criteria; anything else under
// those labels, and every job function or industry, becomes a fixed synthetic value.
var criteriaValues = map[string]map[string]bool{
	"Seniority level": {
		"Internship": true, "Entry level": true, "Associate": true, "Mid-Senior level": true,
		"Director": true, "Executive": true, "Not Applicable": true,
	},
	"Employment type": {
		"Full-time": true, "Part-time": true, "Contract": true, "Temporary": true,
		"Volunteer": true, "Internship": true, "Other": true,
	},
}

var criteriaDefault = map[string]string{
	"Seniority level": "Mid-Senior level",
	"Employment type": "Full-time",
	"Job function":    "Engineering and Information Technology",
	"Industries":      "Software Development",
}

// benefitTexts are LinkedIn's fixed benefit badges.
var benefitTexts = map[string]bool{
	"Actively Hiring": true, "Be an early applicant": true, "Early applicant": true,
	"Medical insurance": true, "Vision insurance": true, "Dental insurance": true,
	"401(k)": true, "Paid maternity leave": true, "Paid paternity leave": true,
	"Tuition assistance": true, "Disability insurance": true, "Commuter benefits": true,
	"Pension plan": true, "Child care support": true, "Student loan assistance": true,
}
