package main_test

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	m "github.com/safanaj/dummy-hostaliases-patcher"

	"context"
	corev1 "k8s.io/api/core/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

var _ = Describe("HostAliases", func() {
	tctx := context.TODO()
	logf.IntoContext(tctx, logf.Log)
	var empty, wrong, correct []corev1.HostAlias
	const correctIP = "1.1.1.1"
	const wrongIP = "2.2.2.2"
	const correctName = "this.is.the.name"
	const correctNameOne = "this.is.the.name.one"
	const correctNameTwo = "this.is.the.name.two"
	const wrongName = "that.is.not.the.name.one"

	BeforeEach(func() {
		empty = []corev1.HostAlias{}
		wrong = []corev1.HostAlias{corev1.HostAlias{IP: wrongIP, Hostnames: []string{wrongName}}}
		correct = []corev1.HostAlias{corev1.HostAlias{IP: correctIP, Hostnames: []string{correctName}}}
	})

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
			It("should not need update", func() {
				ha, r := m.GetDesiredHostAliases(tctx, correctIP, []string{correctName, correctNameOne}, correct)
				Expect(r).To(Equal(true))
				Expect(ha).To(HaveLen(1))
				Expect(ha[0].Hostnames).To(HaveLen(2))
			})
		})

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
