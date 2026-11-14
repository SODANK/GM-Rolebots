package rolebots

// mineralCascade is the minerals the module prices, lists, bids for and reports,
// in the order the ESP pages print them (no reader treats it as a ranking).
// Morphite is included so Mercoxit is not priced at zero (oreValues sums these).
var mineralCascade = []int64{
	typeTritanium, typePyerite, typeMexallon, typeIsogen,
	typeNocxium, typeZydrine, typeMegacyte, typeMorphite,
}
