package main_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	m "github.com/safanaj/dummy-hostaliases-patcher"

	"context"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

func TestDummyHostaliasesPatcher(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "DummyHostaliasesPatcher Suite")
}

var _ = Describe("HostAliases", func() {
	tctx := context.TODO()
	logf.IntoContext(tctx, logf.Log)
	var empty, wrong, correct []corev1.HostAlias
	const correctIP = "1.1.1.1"
	const wrongIP = "2.2.2.2"
	const correctName = "this.is.the.name"
	const correctNameOne = "this.is.the.name.one"
	const wrongName = "that.is.not.the.name.one"

	BeforeEach(func() {
		empty = []corev1.HostAlias{}
		wrong = []corev1.HostAlias{corev1.HostAlias{IP: wrongIP, Hostnames: []string{wrongName}}}
		correct = []corev1.HostAlias{corev1.HostAlias{IP: correctIP, Hostnames: []string{correctName}}}
	})

	// DescribeTable("Checking hostAlias set",
	// 	func(ctx context.Context, correctIP string, correctNames []string, aliases []corev1.HostAlias,
	// 		needsUpdate bool, hostAliasesLen, correctHostnamesLen, wrongHostnamesLen int) {
	// 		ha, r := m.GetDesiredHostAliases(ctx, correctIP, correctNames, aliases)

	// 		// wrIdx := 0
	// 		// coIdx := 0
	// 		// if correctHostnamesLen > 0 || wrongHostnamesLen > 0 {
	// 		// 	for i, a := range ha {
	// 		// 		if a.IP == wrongIP {
	// 		// 			wrIdx = i
	// 		// 		}
	// 		// 		if a.IP == correctIP {
	// 		// 			coIdx = i
	// 		// 		}
	// 		// 	}
	// 		// } else {
	// 		// 	correctHostnamesLen = 1
	// 		// 	wrongHostnamesLen = 1
	// 		// }

	// 		Expect(r).To(Equal(needsUpdate))
	// 		Expect(ha).To(HaveLen(hostAliasesLen))
	// 		// Expect(ha[coIdx].Hostnames).To(HaveLen(correctHostnamesLen))
	// 		// Expect(ha[wrIdx].Hostnames).To(HaveLen(wrongHostnamesLen))
	// 	},
	// 	Entry("When host aliseses is an empty set should need update and have 1 single entry",
	// 		tctx, correctIP, []string{correctName}, empty, true, 1, 1, 0),
	// 	Entry("When we have empty IP should not need update and have the same len as the input",
	// 		tctx, "", []string{correctName}, wrong, false, len(wrong), 0, len(wrong[0].Hostnames)),
	// 	Entry("When we have empty dns names set should not need update and have the same len as the input",
	// 		tctx, correctIP, []string{}, wrong, false, len(wrong), 0, len(wrong[0].Hostnames)),
	// 	Entry("When we have empty IP nd empty dns names set should not need update and have the same len as the input",
	// 		tctx, "", []string{}, wrong, true, len(wrong), -1, -1),
	// 	Entry("When we have IP and dns names to look for should need updte nd added 1 hostaliase with the len as the correct one",
	// 		tctx, correctIP, []string{correctName}, wrong, len(wrong)+1, len(correct[0].Hostnames), len(wrong[0].Hostnames)),

	// 	Entry("When we have only the correct entry",
	// 		tctx, correctIP, []string{correctName}, correct, false, len(correct), len(correct[0].Hostnames), -1),

	// 	Entry("When we have only the correct entry but with a missing hostname should need update and hostnmes len incresed by one",
	// 		tctx, correctIP, []string{correctName, correctNameOne}, correct, true, len(correct), len(correct[0].Hostnames)+1, -1),
	// )

	Describe("Checking empty hostAlias set", func() {
		It("should need update", func() {
			ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName}, empty)
			Expect(r).To(Equal(true))
			Expect(ha).To(HaveLen(1))
		})
	})

	Describe("Checking wrong hostAlias set", func() {

		Context("When we have empty IP", func() {
			It("should not need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, "", []string{correctName}, wrong)
				Expect(r).To(Equal(false))
				Expect(ha).To(HaveLen(1))
			})
		})
		Context("When we have empty dns names set", func() {
			It("should not need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{}, wrong)
				Expect(r).To(Equal(false))
				Expect(ha).To(HaveLen(1))
			})
		})
		Context("When we have empty ip and empty dns names set", func() {
			It("should not need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, "", []string{}, wrong)
				Expect(r).To(Equal(false))
				Expect(ha).To(HaveLen(1))
			})
		})

		Context("When we have IP and dns names to look for", func() {
			It("should need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName}, wrong)
				Expect(r).To(Equal(true))
				Expect(ha).To(HaveLen(2))
				wrIdx := 0
				coIdx := 0
				for i, a := range ha {
					if a.IP == wrongIP {
						wrIdx = i
					}
					if a.IP == correctIP {
						coIdx = i
					}
				}
				Expect(ha[wrIdx].Hostnames).To(HaveLen(1))
				Expect(ha[coIdx].Hostnames).To(HaveLen(1))
			})
		})

		Context("With additional correct dns name", func() {
			BeforeEach(func() {
				wrong[0].Hostnames = append(wrong[0].Hostnames, correctName)
			})
			It("should need update, and cleaned", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName}, wrong)
				Expect(r).To(Equal(true))
				Expect(ha).To(HaveLen(2))
				wrIdx := 0
				coIdx := 0
				for i, a := range ha {
					if a.IP == wrongIP {
						wrIdx = i
					}
					if a.IP == correctIP {
						coIdx = i
					}
				}
				Expect(ha[wrIdx].Hostnames).To(HaveLen(1))
				Expect(ha[coIdx].Hostnames).To(HaveLen(1))
			})
		})
	})

	Describe("Checking wrong hostAlias set", func() {
		Context("With additional 2 correct dns names", func() {
			BeforeEach(func() {
				wrong[0].Hostnames = append(wrong[0].Hostnames, correctName)
				wrong[0].Hostnames = append(wrong[0].Hostnames, correctNameOne)
			})
			It("should need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName, correctNameOne}, wrong)
				Expect(r).To(Equal(true))
				Expect(ha).To(HaveLen(2))
				wrIdx := 0
				coIdx := 0
				for i, a := range ha {
					if a.IP == wrongIP {
						wrIdx = i
					}
					if a.IP == correctIP {
						coIdx = i
					}
				}
				Expect(ha[wrIdx].Hostnames).To(HaveLen(1))
				Expect(ha[coIdx].Hostnames).To(HaveLen(2))
			})
		})
	})

	Describe("Checking correct hostAlias set", func() {
		Context("With only the correct name", func() {
			It("should not need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName}, correct)
				Expect(r).To(Equal(false))
				Expect(ha).To(HaveLen(1))
				Expect(ha[0].Hostnames).To(HaveLen(1))
			})
		})

		Context("With only one of the correct names", func() {
			It("should need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName, correctNameOne}, correct)
				Expect(r).To(Equal(true))
				Expect(ha).To(HaveLen(1))
				Expect(ha[0].Hostnames).To(HaveLen(2))
			})
		})
	})

	Describe("Checking correct hostAlias set", func() {
		Context("With additional wrong dns name", func() {
			BeforeEach(func() {
				correct[0].Hostnames = append(correct[0].Hostnames, wrongName)
			})
			It("should not need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName}, correct)
				Expect(r).To(Equal(false))
				Expect(ha).To(HaveLen(1))
				Expect(ha[0].Hostnames).To(HaveLen(2))
			})
		})
	})

})
